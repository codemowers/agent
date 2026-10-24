package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"unicode"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	authenticationv1 "k8s.io/api/authentication/v1"
	authv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

// TaskWaker records a wake request on a Task.
type TaskWaker interface {
	WakeTask(ctx context.Context, namespace, name, actor, key, text, sourceURL string) error
}

// wakeTask lets an in-cluster caller holding the custom verb "wake" on a Task
// wake it with a projected ServiceAccount token for the audience of the wake
// Service, agent-operator-wake.<namespace>. An Idempotency-Key header makes
// retries safe.
func (h *Handler) wakeTask(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	logger := log.FromContext(r.Context()).WithValues("namespace", namespace, "task", name)
	authorization := r.Header.Get("Authorization")
	if h.Waker == nil || h.TokenReviews == nil || h.AccessReviews == nil || !strings.HasPrefix(authorization, "Bearer ") {
		jsonError(w, errors.New("wake token required"), http.StatusUnauthorized)
		return
	}
	review, err := h.TokenReviews.TokenReviews().Create(r.Context(), &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: strings.TrimPrefix(authorization, "Bearer "), Audiences: []string{agentsv1alpha1.ServiceAudience(agentsv1alpha1.OperatorWakeURL(h.HarnessNamespace))}},
	}, metav1.CreateOptions{})
	if err != nil {
		jsonError(w, errors.New("token review unavailable"), http.StatusServiceUnavailable)
		return
	}
	if !review.Status.Authenticated || review.Status.Error != "" {
		logger.Info("wake rejected", "reason", "token")
		jsonError(w, errors.New("wake token rejected"), http.StatusUnauthorized)
		return
	}
	user := review.Status.User
	extra := make(map[string]authv1.ExtraValue, len(user.Extra))
	for key, values := range user.Extra {
		extra[key] = authv1.ExtraValue(values)
	}
	access, err := h.AccessReviews.SubjectAccessReviews().Create(r.Context(), &authv1.SubjectAccessReview{
		Spec: authv1.SubjectAccessReviewSpec{User: user.Username, UID: user.UID, Groups: user.Groups, Extra: extra,
			ResourceAttributes: &authv1.ResourceAttributes{Group: "codemowers.ai", Resource: "tasks", Verb: "wake", Namespace: namespace, Name: name}},
	}, metav1.CreateOptions{})
	if err != nil || access.Status.EvaluationError != "" {
		jsonError(w, errors.New("access review unavailable"), http.StatusServiceUnavailable)
		return
	}
	if !access.Status.Allowed || access.Status.Denied {
		logger.Info("wake rejected", "reason", "access", "user", user.Username)
		jsonError(w, errors.New("wake on Task denied"), http.StatusForbidden)
		return
	}
	var req struct{ Text, URL string }
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&req); err != nil {
		jsonError(w, errors.New("invalid JSON body"), http.StatusBadRequest)
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if !wakeText(req.Text, 4096) || len(key) > 200 || strings.ContainsFunc(key, func(r rune) bool { return !unicode.IsPrint(r) }) ||
		req.URL != "" && !wakeURL(req.URL) {
		jsonError(w, errors.New("invalid text, URL or Idempotency-Key"), http.StatusBadRequest)
		return
	}
	if req.Text == "" {
		req.Text = "Wake request"
	}
	if err := h.Waker.WakeTask(r.Context(), namespace, name, user.Username, key, req.Text, req.URL); err != nil {
		if apierrors.IsNotFound(err) {
			jsonError(w, errors.New("Task not found"), http.StatusNotFound)
			return
		}
		logger.Error(err, "wake failed", "user", user.Username)
		jsonError(w, errors.New("wake unavailable"), http.StatusServiceUnavailable)
		return
	}
	logger.Info("wake accepted", "user", user.Username, "idempotent", key != "")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]string{"status": "accepted"})
}

// wakeText accepts bounded printable text with line breaks.
func wakeText(s string, limit int) bool {
	if len(s) > limit {
		return false
	}
	for _, r := range s {
		if !unicode.IsPrint(r) && r != '\n' && r != '\t' {
			return false
		}
	}
	return true
}

func wakeURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && len(raw) <= 2048 && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil
}
