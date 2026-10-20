package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

const MaxConversationMessages = 64

// AppendMessage keeps Task status small and makes replayed worker results idempotent.
func (s *TaskStatus) AppendMessage(message TaskMessage) bool {
	for _, existing := range s.Conversation {
		if existing.ID == message.ID {
			return false
		}
	}
	if len(message.Content) > 8000 {
		message.Content = message.Content[:8000]
	}
	s.Conversation = append(s.Conversation, message)
	if (message.Role == "user" || message.Role == "external" || message.Role == "agent") &&
		!message.CreatedAt.IsZero() && (s.LastMessageAt == nil || message.CreatedAt.After(s.LastMessageAt.Time)) {
		at := metav1.NewTime(message.CreatedAt.Time)
		s.LastMessageAt = &at
	}
	if len(s.Conversation) > MaxConversationMessages {
		s.Conversation = s.Conversation[len(s.Conversation)-MaxConversationMessages:]
	}
	return true
}
