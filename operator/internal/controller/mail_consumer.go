package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	amqp "github.com/rabbitmq/amqp091-go"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type mailDelivery struct {
	Version     int                 `json:"version"`
	Namespace   string              `json:"namespace"`
	Mailbox     string              `json:"mailbox"`
	MailboxUID  string              `json:"mailboxUID"`
	Folder      string              `json:"folder"`
	UIDValidity int64               `json:"uidValidity"`
	UID         int64               `json:"uid"`
	ThreadID    string              `json:"threadId,omitempty"`
	Headers     map[string][]string `json:"headers"`
	Checkpoint  bool                `json:"checkpoint,omitempty"`
}

func mailDeliveryID(event mailDelivery) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d\x00%d", event.MailboxUID, event.Folder, event.UIDValidity, event.UID)))
	return hex.EncodeToString(sum[:])
}

func (c *EventConsumer) handleMail(ctx context.Context, d amqp.Delivery) error {
	if len(d.Body) > 64*1024 {
		return nil
	}
	namespace, _ := d.Headers["x-namespace"].(string)
	mailboxName, _ := d.Headers["x-mailbox"].(string)
	mailboxUID, _ := d.Headers["x-mailbox-uid"].(string)
	// RabbitMQ verified user_id against the publisher's token, so the event
	// came from a Mailbox Pod of this namespace.
	if namespace == "" || d.UserId != agentsv1alpha1.ServiceAccountUser(namespace, MailboxServiceAccount) {
		return nil
	}
	var event mailDelivery
	if json.Unmarshal(d.Body, &event) != nil || event.Version != 1 || event.Namespace != namespace || event.Mailbox != mailboxName || event.MailboxUID != mailboxUID || event.UID < 0 || event.UIDValidity < 1 || event.Folder == "" || len(event.ThreadID) > 256 || d.RoutingKey != "mail."+namespace+"."+mailboxName || d.MessageId != mailDeliveryID(event) || (event.UID == 0 && !event.Checkpoint) {
		return nil
	}
	if len(event.Headers) > 128 {
		return nil
	}
	for name, values := range event.Headers {
		if len(name) > 80 || len(values) > 4 {
			return nil
		}
		for _, value := range values {
			if len(value) > 4096 {
				return nil
			}
		}
	}
	var mailbox agentsv1alpha1.Mailbox
	if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: mailboxName}, &mailbox); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	if string(mailbox.UID) != mailboxUID || mailbox.Spec.IMAP == nil || mailbox.Spec.IMAP.Folder != "" && mailbox.Spec.IMAP.Folder != event.Folder {
		return nil
	}
	if cursor := mailbox.Status.IMAP; cursor != nil && cursor.Cursor != nil && cursor.Cursor.Folder == event.Folder {
		if cursor.Cursor.UIDValidity > event.UIDValidity || cursor.Cursor.UIDValidity == event.UIDValidity && cursor.Cursor.LastUID >= event.UID {
			return nil
		}
	}
	if event.Checkpoint {
		return c.advanceMailCursor(ctx, &mailbox, event)
	}
	var bindings agentsv1alpha1.MailBindingList
	if err := c.APIReader.List(ctx, &bindings, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range bindings.Items {
		binding := &bindings.Items[i]
		if binding.Spec.MailboxRef != mailboxName {
			continue
		}
		var agent agentsv1alpha1.Agent
		if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: binding.Spec.AgentRef}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		matchedEvent, matched, err := matchMailBinding(binding.Spec, mailMetadata{Headers: event.Headers})
		if err != nil {
			return fmt.Errorf("MailBinding %s/%s: %w", namespace, binding.Name, err)
		}
		if !matched {
			continue
		}
		text := matchedEvent.Text
		if id, ok := singleMailHeader(event.Headers, "Message-ID"); ok {
			text += " (Message-ID: " + id + ")"
		}
		selected, err := c.routeEvent(ctx, &agent, intakeEvent{Namespace: namespace, Source: "imap", Type: matchedEvent.Type,
			Actor: matchedEvent.Actor, Text: text, URL: matchedEvent.URL, Key: matchedEvent.Key,
			FallbackTask: binding.Spec.Routing.Otherwise.TaskRef, Receipt: d.MessageId + "-" + string(binding.UID), Binding: string(binding.UID)})
		if err != nil {
			return err
		}
		if root := threadRootMessageID(event.Headers); selected != nil && root != "" {
			if err := c.ensureExternalReference(ctx, selected, agentsv1alpha1.ExternalReference{
				Source: "imap", Kind: "thread", ID: root, MailboxRef: event.Mailbox,
				MailboxUID: event.MailboxUID, Folder: event.Folder, IMAPThreadID: event.ThreadID,
			}); err != nil {
				return err
			}
		}
	}
	return c.advanceMailCursor(ctx, &mailbox, event)
}

func (c *EventConsumer) ensureExternalReference(ctx context.Context, task *agentsv1alpha1.Task, ref agentsv1alpha1.ExternalReference) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current agentsv1alpha1.Task
		if err := c.Client.Get(ctx, client.ObjectKeyFromObject(task), &current); err != nil {
			return err
		}
		if current.UID != task.UID {
			return nil
		}
		for i, existing := range current.Status.ExternalReferences {
			if existing.Source == ref.Source && existing.ServerURL == ref.ServerURL && existing.MailboxUID == ref.MailboxUID && existing.Folder == ref.Folder && existing.Kind == ref.Kind && existing.ID == ref.ID {
				if existing.IMAPThreadID == "" && ref.IMAPThreadID != "" {
					current.Status.ExternalReferences[i].IMAPThreadID = ref.IMAPThreadID
					return c.Client.Status().Update(ctx, &current)
				}
				return nil
			}
		}
		current.Status.ExternalReferences = append(current.Status.ExternalReferences, ref)
		return c.Client.Status().Update(ctx, &current)
	})
}

func (c *EventConsumer) advanceMailCursor(ctx context.Context, mailbox *agentsv1alpha1.Mailbox, event mailDelivery) error {
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		var current agentsv1alpha1.Mailbox
		if err := c.Client.Get(ctx, client.ObjectKey{Namespace: event.Namespace, Name: event.Mailbox}, &current); err != nil {
			return err
		}
		if current.UID != mailbox.UID {
			return nil
		}
		if current.Status.IMAP == nil {
			current.Status.IMAP = &agentsv1alpha1.MailIMAPStatus{}
		}
		previous := current.Status.IMAP.Cursor
		if previous != nil && previous.Folder == event.Folder {
			if previous.UIDValidity > event.UIDValidity || previous.UIDValidity == event.UIDValidity && previous.LastUID >= event.UID {
				return nil
			}
		}
		current.Status.IMAP.Cursor = &agentsv1alpha1.MailCursor{Folder: event.Folder, UIDValidity: event.UIDValidity, LastUID: event.UID}
		now := metav1.Now()
		current.Status.IMAP.LastCheckedAt = &now
		if !event.Checkpoint {
			current.Status.IMAP.LastReceivedAt = &now
		}
		current.Status.IMAP.State = "connected"
		return c.Client.Status().Update(ctx, &current)
	})
}
