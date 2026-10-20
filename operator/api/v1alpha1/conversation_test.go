package v1alpha1

import (
	"fmt"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestConversationDeduplicatesAndBoundsHistory(t *testing.T) {
	var status TaskStatus
	for i := 0; i < MaxConversationMessages+2; i++ {
		if !status.AppendMessage(TaskMessage{ID: fmt.Sprint(i), Content: "turn"}) {
			t.Fatal("new turn rejected")
		}
	}
	if len(status.Conversation) != MaxConversationMessages || status.Conversation[0].ID != "2" {
		t.Fatalf("unexpected bounded conversation: %+v", status.Conversation)
	}
	if status.AppendMessage(TaskMessage{ID: "2", Content: "duplicate"}) {
		t.Fatal("duplicate turn accepted")
	}
}

func TestLastMessageTimeSurvivesSystemHistory(t *testing.T) {
	actual := metav1.NewTime(time.Date(2026, 10, 19, 8, 0, 0, 0, time.UTC))
	later := metav1.NewTime(actual.Add(time.Minute))
	var status TaskStatus
	status.AppendMessage(TaskMessage{ID: "mail", Role: "external", CreatedAt: actual})
	for i := 0; i < MaxConversationMessages+1; i++ {
		status.AppendMessage(TaskMessage{ID: fmt.Sprintf("system-%d", i), Role: "system", CreatedAt: later})
	}
	if status.LastMessageAt == nil || !status.LastMessageAt.Equal(&actual) {
		t.Fatalf("system history changed last actual message: %+v", status.LastMessageAt)
	}
	status.AppendMessage(TaskMessage{ID: "agent", Role: "agent", CreatedAt: later})
	if status.LastMessageAt == nil || !status.LastMessageAt.Equal(&later) {
		t.Fatalf("agent response did not advance last message: %+v", status.LastMessageAt)
	}
}
