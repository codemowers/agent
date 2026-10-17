package controller

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	amqp "github.com/rabbitmq/amqp091-go"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestEventScopeAndServerMatching(t *testing.T) {
	task := &agentsv1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "team-a",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "developer", UID: types.UID("current-agent"),
			}},
		},
		Spec: agentsv1alpha1.TaskSpec{ExternalReferences: []agentsv1alpha1.ExternalReference{{
			Source: "gitea", ServerURL: "https://forge.example.test", RepositoryOwner: "team", RepositoryName: "project", Kind: "issue", ID: "8",
		}}},
	}
	if !ownedByAgent(task, "developer", types.UID("current-agent")) ||
		ownedByAgent(task, "developer", types.UID("previous-agent")) ||
		ownedByAgent(task, "support", types.UID("current-agent")) {
		t.Fatal("Task Agent ownership was not enforced")
	}
	event, ok := parseRoutingKey("gitea.forge.example.test:team/project#issue:8")
	if !ok || !refMatches(task, event) {
		t.Fatal("matching Gitea server did not route")
	}
	other, ok := parseRoutingKey("gitea.other.example.test:team/project#issue:8")
	if !ok || refMatches(task, other) {
		t.Fatal("event from another Gitea server crossed the Task boundary")
	}
	publicGithub, ok := parseRoutingKey("github:team/project#issue:8")
	if !ok {
		t.Fatal("public GitHub routing key rejected")
	}
	task.Spec.ExternalReferences[0].Source = "github"
	task.Spec.ExternalReferences[0].ServerURL = "https://github.com"
	if !refMatches(task, publicGithub) {
		t.Fatal("public GitHub URL did not match its routing key")
	}
}

func TestAgentWebhookEventRoutesOnlySubscribedAgent(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	var objects []client.Object
	for _, item := range []struct{ namespace, name, host, hookID string }{
		{"team-a", "developer", "https://github.com", "42"},
		{"team-b", "support", "https://github.com", "42"},
		{"team-c", "other", "https://ghe.example.test", "42"},
	} {
		uid := types.UID(item.name + "-uid")
		agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: item.name, Namespace: item.namespace, UID: uid},
			Spec: agentsv1alpha1.AgentSpec{GitHubWebhooks: []agentsv1alpha1.GitHubWebhook{{ServerURL: item.host, HookID: item.hookID, SecretRef: "org-hook"}}, GitProviders: []agentsv1alpha1.GitProvider{{Type: "github", ServerURL: item.host}}}}
		task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "issue-8", Namespace: item.namespace,
			Labels:          map[string]string{"codemowers.cloud/agent": item.name},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: item.name, UID: uid}}},
			Spec: agentsv1alpha1.TaskSpec{ExternalReferences: []agentsv1alpha1.ExternalReference{{Source: "github", ServerURL: item.host,
				RepositoryOwner: "codemowers", RepositoryName: "agent", Kind: "issue", ID: "8"}}}}
		objects = append(objects, agent, task, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "org-hook", Namespace: item.namespace},
			Data: map[string][]byte{"token": []byte("shared-hmac-key")}})
	}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	consumer := &EventConsumer{Client: k8s, APIReader: k8s}
	delivery := amqp.Delivery{RoutingKey: "github:codemowers/agent#issue:8", MessageId: "delivery-1",
		Body:    []byte(`{"repository":{"full_name":"codemowers/agent"},"issue":{"number":8}}`),
		Headers: amqp.Table{"x-namespace": "team-a", "x-agent": "developer", "x-hook-id": "42", "x-source": "github", "x-event": "issues"}}
	mac := hmac.New(sha256.New, []byte("shared-hmac-key"))
	for _, field := range []string{"team-a", "developer", "42", "github", "issues", delivery.RoutingKey, delivery.MessageId} {
		mac.Write([]byte(field))
		mac.Write([]byte{0})
	}
	mac.Write(delivery.Body)
	delivery.Headers["x-source-signature"] = hex.EncodeToString(mac.Sum(nil))
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	// A second namespace with its own copy of the signing key receives the
	// same GitHub delivery independently.
	second := delivery
	second.Headers = amqp.Table{"x-namespace": "team-b", "x-agent": "support", "x-hook-id": "42", "x-source": "github", "x-event": "issues"}
	secondMAC := hmac.New(sha256.New, []byte("shared-hmac-key"))
	for _, field := range []string{"team-b", "support", "42", "github", "issues", second.RoutingKey, second.MessageId} {
		secondMAC.Write([]byte(field))
		secondMAC.Write([]byte{0})
	}
	secondMAC.Write(second.Body)
	second.Headers["x-source-signature"] = hex.EncodeToString(secondMAC.Sum(nil))
	if err := consumer.handle(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	for _, item := range []struct {
		namespace string
		want      bool
	}{{"team-a", true}, {"team-b", true}, {"team-c", false}} {
		var task agentsv1alpha1.Task
		if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: item.namespace, Name: "issue-8"}, &task); err != nil {
			t.Fatal(err)
		}
		if got := task.Annotations["codemowers.cloud/wake-pending"] == "true"; got != item.want {
			t.Fatalf("%s wake=%v, want %v", item.namespace, got, item.want)
		}
	}
	delivery.RoutingKey = "github:codemowers/other#issue:8"
	parsed, _ := parseRoutingKey(delivery.RoutingKey)
	if _, err := consumer.verifyAgentEvent(context.Background(), delivery, parsed); err == nil {
		t.Fatal("forged routing key was accepted with the original source signature")
	}
	delivery.RoutingKey = "github:codemowers/agent#issue:8"
	var wrongSecret corev1.Secret
	if err := k8s.Get(context.Background(), client.ObjectKey{Namespace: "team-b", Name: "org-hook"}, &wrongSecret); err != nil {
		t.Fatal(err)
	}
	wrongSecret.Data["token"] = []byte("wrong-hmac-key")
	if err := k8s.Update(context.Background(), &wrongSecret); err != nil {
		t.Fatal(err)
	}
	parsed, _ = parseRoutingKey(delivery.RoutingKey)
	if _, err := consumer.verifyAgentEvent(context.Background(), second, parsed); err == nil {
		t.Fatal("event crossed into a namespace without the matching signing key")
	}
	if err := k8s.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "org-hook", Namespace: "team-a"}}); err != nil {
		t.Fatal(err)
	}
	if err := consumer.handle(context.Background(), delivery); !errors.Is(err, errSourceCredentialUnavailable) {
		t.Fatalf("missing signing credential should retry delivery, got %v", err)
	}
}

func TestTaskDeliveryReceiptSurvivesInterleavedEvents(t *testing.T) {
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		"codemowers.cloud/initial-delivery-id": "delivery-0",
		"codemowers.cloud/event-message-id":    "delivery-3",
		"codemowers.cloud/delivery-receipts":   `["delivery-1","delivery-2","delivery-3"]`,
	}}}
	for _, id := range []string{"delivery-0", "delivery-1", "delivery-2", "delivery-3"} {
		if !taskHasDelivery(task, id) {
			t.Fatalf("redelivery %s was not recognized", id)
		}
	}
	if taskHasDelivery(task, "delivery-4") {
		t.Fatal("new delivery was mistaken for a duplicate")
	}
}
