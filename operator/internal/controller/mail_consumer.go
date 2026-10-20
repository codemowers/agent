package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

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
	if json.Unmarshal(d.Body, &event) != nil || event.Version != 1 || event.Namespace != namespace || event.Mailbox != mailboxName || event.MailboxUID != mailboxUID || event.UID < 0 || event.UIDValidity < 1 || event.Folder == "" || len(event.ThreadID) > 256 || d.RoutingKey != "mailbox."+namespace+"."+mailboxName || d.MessageId != mailDeliveryID(event) || (event.UID == 0 && !event.Checkpoint) {
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
	var subscriptions agentsv1alpha1.MailSubscriptionList
	if err := c.APIReader.List(ctx, &subscriptions, client.InNamespace(namespace)); err != nil {
		return err
	}
	for i := range subscriptions.Items {
		sub := &subscriptions.Items[i]
		if sub.Spec.MailboxRef != mailboxName {
			continue
		}
		var agent agentsv1alpha1.Agent
		if err := c.APIReader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: sub.Spec.AgentRef}, &agent); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return err
		}
		matchedEvent, matched, err := matchMailSubscription(sub.Spec, mailMetadata{Headers: event.Headers})
		if err != nil {
			return fmt.Errorf("MailSubscription %s/%s: %w", namespace, sub.Name, err)
		}
		if !matched {
			continue
		}
		var tasks agentsv1alpha1.TaskList
		if err := c.APIReader.List(ctx, &tasks, client.InNamespace(namespace), client.MatchingLabels{"codemowers.cloud/agent": agent.Name}); err != nil {
			return err
		}
		var selected *agentsv1alpha1.Task
		for j := range tasks.Items {
			task := &tasks.Items[j]
			if ownedByAgent(task, agent.Name, agent.UID) && routingKeyMatches(task, matchedEvent.Key) {
				if selected != nil {
					return fmt.Errorf("multiple Tasks claim mail key for Agent %s/%s", namespace, agent.Name)
				}
				selected = task
			}
		}
		if selected == nil && sub.Spec.Routing.Otherwise.TaskRef != "" {
			for j := range tasks.Items {
				task := &tasks.Items[j]
				if task.Name == sub.Spec.Routing.Otherwise.TaskRef && ownedByAgent(task, agent.Name, agent.UID) {
					selected = task
					break
				}
			}
		}
		if selected == nil {
			continue
		}
		receipt := d.MessageId + "-" + string(sub.UID)
		if taskHasDelivery(selected, receipt) {
			continue
		}
		if routingKeyMatches(selected, matchedEvent.Key) {
			root := threadRootMessageID(event.Headers)
			if root != "" {
				if err := c.ensureExternalReference(ctx, selected, agentsv1alpha1.ExternalReference{
					Source: "imap", Kind: "thread", ID: root, MailboxRef: event.Mailbox,
					MailboxUID: event.MailboxUID, Folder: event.Folder, IMAPThreadID: event.ThreadID,
				}); err != nil {
					return err
				}
			}
		}
		var coalesce time.Duration
		if sub.Spec.Routing.CoalesceFor != "" {
			coalesce, err = time.ParseDuration(sub.Spec.Routing.CoalesceFor)
			if err != nil || coalesce < time.Second || coalesce > time.Hour {
				return fmt.Errorf("invalid mail coalesceFor on %s/%s", namespace, sub.Name)
			}
		}
		text := matchedEvent.Text
		if id, ok := singleMailHeader(event.Headers, "Message-ID"); ok {
			text += " (Message-ID: " + id + ")"
		}
		if err := c.wakeTaskWithContent(ctx, selected, strings.Join(matchedEvent.Key, " / "), receipt, "imap", matchedEvent.Actor, text, matchedEvent.URL, coalesce); err != nil {
			return err
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
