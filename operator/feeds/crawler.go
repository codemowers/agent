package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/mmcdole/gofeed"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

const (
	userAgent       = "codemowers-agent (+https://github.com/codemowers/agent)"
	defaultInterval = time.Hour
	maxFeedBytes    = 8 << 20
	maxItems        = 100
	maxWakeItems    = 20
	retryAfter      = time.Minute
)

// FeedReconciler polls each Feed on its interval and wakes the Tasks of its
// FeedBindings once per poll that finds new items. Status advances only after
// every wake succeeded, so a failure repeats on the next poll.
type FeedReconciler struct {
	client.Client
	HTTP      *http.Client
	WakeURL   string // operator wake endpoint base, without the path
	TokenFile string
}

func (r *FeedReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var feed agentsv1alpha1.Feed
	if err := r.Get(ctx, req.NamespacedName, &feed); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	interval := defaultInterval
	if feed.Spec.Interval != nil && feed.Spec.Interval.Duration >= time.Minute {
		interval = feed.Spec.Interval.Duration
	}
	if polled := feed.Status.LastPolledAt; polled != nil {
		if wait := time.Until(polled.Add(interval)); wait > 0 {
			return ctrl.Result{RequeueAfter: wait}, nil
		}
	}
	err := r.poll(ctx, &feed)
	condition := metav1.Condition{Type: "Ready", Status: metav1.ConditionTrue, Reason: "Polled", Message: "Feed polled"}
	if err != nil {
		condition = metav1.Condition{Type: "Ready", Status: metav1.ConditionFalse, Reason: "PollFailed", Message: truncate(err.Error(), 512)}
	}
	condition.ObservedGeneration = feed.Generation
	meta.SetStatusCondition(&feed.Status.Conditions, condition)
	if err != nil {
		// LastPolledAt stays, so the next attempt comes after retryAfter.
		if updateErr := r.Status().Update(ctx, &feed); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
		ctrl.LoggerFrom(ctx).Info("feed poll failed", "error", err.Error())
		return ctrl.Result{RequeueAfter: retryAfter}, nil
	}
	now := metav1.Now()
	feed.Status.LastPolledAt = &now
	if err := r.Status().Update(ctx, &feed); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: interval}, nil
}

// poll fetches the feed and updates its status fields in place. It returns
// an error, leaving the cursor unchanged, when the fetch or a wake failed.
func (r *FeedReconciler) poll(ctx context.Context, feed *agentsv1alpha1.Feed) error {
	if !strings.HasPrefix(feed.Spec.URL, "https://") {
		return errors.New("feed URL must use https")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, feed.Spec.URL, nil)
	if err != nil {
		return err
	}
	request.Header.Set("User-Agent", userAgent)
	if feed.Status.ETag != "" {
		request.Header.Set("If-None-Match", feed.Status.ETag)
	}
	if feed.Status.LastModified != "" {
		request.Header.Set("If-Modified-Since", feed.Status.LastModified)
	}
	response, err := r.HTTP.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return nil
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("feed returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxFeedBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxFeedBytes {
		return errors.New("feed exceeds 8 MiB")
	}
	parsed, err := gofeed.NewParser().Parse(bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("parse feed: %w", err)
	}
	now := metav1.Now()
	previous := make(map[string]agentsv1alpha1.FeedItem, len(feed.Status.Items))
	for _, item := range feed.Status.Items {
		previous[item.ID] = item
	}
	// Items outlive the feed's window, so none stored means none recorded yet.
	seeded := len(feed.Status.Items) == 0
	var items []agentsv1alpha1.FeedItem
	var fresh []*gofeed.Item
	seen := map[string]bool{}
	for _, item := range parsed.Items {
		id := itemID(item)
		if seen[id] {
			continue
		}
		seen[id] = true
		stored := agentsv1alpha1.FeedItem{ID: id, Title: plainText(item.Title, 300), Link: truncate(item.Link, 2048), FirstSeen: now}
		if published := item.PublishedParsed; published != nil {
			stored.Published = &metav1.Time{Time: *published}
		} else if updated := item.UpdatedParsed; updated != nil {
			stored.Published = &metav1.Time{Time: *updated}
		}
		if old, ok := previous[id]; ok {
			stored.FirstSeen = old.FirstSeen
		} else if !seeded {
			fresh = append(fresh, item)
		}
		items = append(items, stored)
	}
	// Items that left the feed's window stay until the cap, so short feeds
	// keep a longer history.
	for _, old := range feed.Status.Items {
		if !seen[old.ID] {
			items = append(items, old)
		}
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].FirstSeen.After(items[j].FirstSeen.Time) })
	if len(items) > maxItems {
		items = items[:maxItems]
	}
	// The first poll records the existing items without waking anyone.
	if len(fresh) > 0 {
		if err := r.wakeBindings(ctx, feed, parsed.Title, now, fresh); err != nil {
			return err
		}
		feed.Status.LastChangedAt = &now
	}
	feed.Status.Items = items
	feed.Status.ETag = response.Header.Get("ETag")
	feed.Status.LastModified = response.Header.Get("Last-Modified")
	return nil
}

func (r *FeedReconciler) wakeBindings(ctx context.Context, feed *agentsv1alpha1.Feed, title string, seen metav1.Time, items []*gofeed.Item) error {
	var bindings agentsv1alpha1.FeedBindingList
	if err := r.List(ctx, &bindings, client.InNamespace(feed.Namespace)); err != nil {
		return err
	}
	if title = plainText(title, 200); title == "" {
		title = feed.Name
	}
	text, key := wakeText(title, seen.Time, items), wakeKey(feed.Spec.URL, items)
	for _, binding := range bindings.Items {
		if binding.Spec.FeedRef != feed.Name {
			continue
		}
		if err := r.wake(ctx, feed.Namespace, binding.Spec.TaskRef, key, text, items[0].Link); err != nil {
			return fmt.Errorf("wake Task %s: %w", binding.Spec.TaskRef, err)
		}
	}
	return nil
}

func (r *FeedReconciler) wake(ctx context.Context, namespace, task, key, text, link string) error {
	token, err := os.ReadFile(r.TokenFile)
	if err != nil {
		return err
	}
	body := map[string]string{"text": text}
	if u, err := url.Parse(link); err == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && len(link) <= 2048 {
		body["url"] = link
	}
	payload, _ := json.Marshal(body)
	endpoint := strings.TrimRight(r.WakeURL, "/") + "/internal/v1/namespaces/" + url.PathEscape(namespace) + "/tasks/" + url.PathEscape(task) + "/wake"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)
	response, err := (&http.Client{Timeout: 30 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		message, _ := io.ReadAll(io.LimitReader(response.Body, 512))
		return fmt.Errorf("wake returned HTTP %d: %s", response.StatusCode, strings.TrimSpace(string(message)))
	}
	return nil
}

// itemID prefers the feed's own ID, then the link, then a content hash.
func itemID(item *gofeed.Item) string {
	id := item.GUID
	if id == "" {
		id = item.Link
	}
	if id == "" || len(id) > 1024 {
		sum := sha256.Sum256([]byte(id + "\n" + item.Title + "\n" + item.Published))
		return "sha256:" + hex.EncodeToString(sum[:])
	}
	return id
}

// wakeKey is stable across retries of the same poll, so a repeated wake is
// recorded once.
func wakeKey(feedURL string, items []*gofeed.Item) string {
	ids := make([]string, len(items))
	for i, item := range items {
		ids[i] = itemID(item)
	}
	sum := sha256.Sum256([]byte(feedURL + "\n" + strings.Join(ids, "\n")))
	return "feed-" + hex.EncodeToString(sum[:16])
}

// wakeText lists the new items' titles and links with the time they were
// first seen, which the feeds MCP accepts as a since cursor; the worker
// follows the links it finds relevant.
func wakeText(title string, seen time.Time, items []*gofeed.Item) string {
	noun := "items"
	if len(items) == 1 {
		noun = "item"
	}
	lines := []string{fmt.Sprintf("%s: %d new %s, first seen %s", title, len(items), noun, seen.UTC().Format(time.RFC3339))}
	for i, item := range items {
		if i == maxWakeItems {
			lines = append(lines, fmt.Sprintf("and %d more", len(items)-maxWakeItems))
			break
		}
		line := plainText(item.Title, 300)
		if item.Link != "" {
			line += " — " + truncate(item.Link, 500)
		}
		lines = append(lines, line)
	}
	return truncate(strings.Join(lines, "\n"), 4096)
}

var tags = regexp.MustCompile(`<[^>]*>`)

func plainText(s string, limit int) string {
	s = html.UnescapeString(tags.ReplaceAllString(s, ""))
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool { return r == '\n' || r == '\r' || r == '\t' || r == ' ' }), " ")
	return truncate(s, limit)
}

func truncate(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	for limit > 0 && (s[limit]&0xC0) == 0x80 {
		limit--
	}
	return s[:limit]
}

// publicHTTPClient reaches only public addresses, so a Feed URL or redirect
// cannot reach cluster or host services.
func publicHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		ip := net.ParseIP(host)
		if ip == nil || !publicIP(ip) {
			return fmt.Errorf("address %s is not public", host)
		}
		return nil
	}}
	transport := &http.Transport{DialContext: dialer.DialContext, Proxy: nil, TLSHandshakeTimeout: 10 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second, MaxIdleConns: 10, IdleConnTimeout: 90 * time.Second}
	return &http.Client{Transport: transport, Timeout: time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many redirects")
		}
		if req.URL.Scheme != "https" {
			return errors.New("redirect must use https")
		}
		return nil
	}}
}

var sharedAddressSpace = net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func publicIP(ip net.IP) bool {
	return ip.IsGlobalUnicast() && !ip.IsPrivate() && !sharedAddressSpace.Contains(ip)
}
