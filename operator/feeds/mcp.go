package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	authnv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	authenticationclient "k8s.io/client-go/kubernetes/typed/authentication/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

// feedsMCP serves the recent items of the Feeds in a worker's own namespace.
// Feeds are public, so namespace alone scopes access.
type feedsMCP struct {
	// audience is this Service's own, agent-feeds.<namespace>.
	audience string
	reader   client.Reader
	reviews  authenticationclient.AuthenticationV1Interface
}

type mcpRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	} `json:"params"`
}

func tool(name, description string, properties map[string]any) map[string]any {
	return map[string]any{"name": name, "description": description, "inputSchema": map[string]any{
		"type": "object", "properties": properties, "additionalProperties": false}}
}

var mcpTools = []map[string]any{
	tool("list_feeds", "List the public feeds of this worker's namespace with their last poll and change times", map[string]any{}),
	tool("list_feed_items", "List recent feed items, newest first by the time the crawler first saw them", map[string]any{
		"feed":  map[string]any{"type": "string", "description": "Feed name; omit for all feeds"},
		"since": map[string]any{"type": "string", "format": "date-time", "description": "Only items first seen after this time, such as the time in a wake or your last cut-off"},
		"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "Default 50"}}),
}

func (s *feedsMCP) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/mcp" {
		http.NotFound(w, r)
		return
	}
	namespace, err := s.authenticate(r)
	if err != nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	var request mcpRequest
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&request) != nil || request.JSONRPC != "2.0" {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if strings.HasPrefix(request.Method, "notifications/") {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
	switch request.Method {
	case "initialize":
		response["result"] = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "agent-feeds", "title": "Feeds", "version": "0.1.0"},
			"instructions": "Recent items of the public feeds in this worker's namespace, with title, link, published and firstSeen times. " +
				"Pass the time in a feed wake or your last cut-off as since to get only newer items, then read the linked pages you need."}
	case "tools/list":
		response["result"] = map[string]any{"tools": mcpTools}
	case "tools/call":
		response["result"] = s.call(r.Context(), namespace, request.Params.Name, request.Params.Arguments)
	default:
		response["error"] = map[string]any{"code": -32601, "message": "method not found"}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(response)
}

func (s *feedsMCP) authenticate(r *http.Request) (string, error) {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", errors.New("missing bearer token")
	}
	review, err := s.reviews.TokenReviews().Create(r.Context(), &authnv1.TokenReview{
		Spec: authnv1.TokenReviewSpec{Token: token, Audiences: []string{s.audience}}}, metav1.CreateOptions{})
	if err != nil || !review.Status.Authenticated || review.Status.Error != "" {
		return "", errors.New("token rejected")
	}
	parts := strings.Split(review.Status.User.Username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" || parts[2] == "" {
		return "", errors.New("not a service account")
	}
	return parts[2], nil
}

func mcpText(value any, failed bool) map[string]any {
	text, _ := json.Marshal(value)
	if message, ok := value.(string); ok {
		text = []byte(message)
	}
	return map[string]any{"content": []map[string]any{{"type": "text", "text": string(text)}}, "isError": failed}
}

func (s *feedsMCP) call(ctx context.Context, namespace, name string, raw json.RawMessage) map[string]any {
	var feeds agentsv1alpha1.FeedList
	if err := s.reader.List(ctx, &feeds, client.InNamespace(namespace)); err != nil {
		return mcpText("Feed inventory unavailable", true)
	}
	switch name {
	case "list_feeds":
		list := make([]map[string]any, 0, len(feeds.Items))
		for _, feed := range feeds.Items {
			entry := map[string]any{"name": feed.Name, "url": feed.Spec.URL, "items": len(feed.Status.Items),
				"lastPolledAt": feed.Status.LastPolledAt, "lastChangedAt": feed.Status.LastChangedAt}
			for _, condition := range feed.Status.Conditions {
				if condition.Type == "Ready" && condition.Status != metav1.ConditionTrue {
					entry["error"] = condition.Message
				}
			}
			list = append(list, entry)
		}
		return mcpText(list, false)
	case "list_feed_items":
		var args struct {
			Feed  string `json:"feed"`
			Since string `json:"since"`
			Limit int    `json:"limit"`
		}
		if len(raw) > 0 && json.Unmarshal(raw, &args) != nil {
			return mcpText("Invalid arguments", true)
		}
		var since time.Time
		if args.Since != "" {
			parsed, err := time.Parse(time.RFC3339, args.Since)
			if err != nil {
				return mcpText("since must be an RFC 3339 time", true)
			}
			since = parsed
		}
		if args.Limit < 1 || args.Limit > 200 {
			args.Limit = 50
		}
		type entry struct {
			Feed      string       `json:"feed"`
			Title     string       `json:"title,omitempty"`
			Link      string       `json:"link,omitempty"`
			Published *metav1.Time `json:"published,omitempty"`
			FirstSeen metav1.Time  `json:"firstSeen"`
		}
		var items []entry
		found := args.Feed == ""
		for _, feed := range feeds.Items {
			if args.Feed != "" && feed.Name != args.Feed {
				continue
			}
			found = true
			for _, item := range feed.Status.Items {
				if item.FirstSeen.After(since) {
					items = append(items, entry{Feed: feed.Name, Title: item.Title, Link: item.Link, Published: item.Published, FirstSeen: item.FirstSeen})
				}
			}
		}
		if !found {
			return mcpText("No feed named "+args.Feed+" in this namespace", true)
		}
		sort.SliceStable(items, func(i, j int) bool { return items[i].FirstSeen.After(items[j].FirstSeen.Time) })
		if len(items) > args.Limit {
			items = items[:args.Limit]
		}
		if items == nil {
			items = []entry{}
		}
		return mcpText(items, false)
	}
	return mcpText("Unknown tool "+name, true)
}
