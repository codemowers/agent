package controller

import (
	"context"
	"encoding/json"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	amqp "github.com/rabbitmq/amqp091-go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func mailboxDelivery(t *testing.T, event mailDelivery) amqp.Delivery {
	t.Helper()
	body, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	return amqp.Delivery{Body: body, MessageId: mailDeliveryID(event), RoutingKey: "mail." + event.Namespace + "." + event.Mailbox,
		UserId: agentsv1alpha1.ServiceAccountUser(event.Namespace, MailboxServiceAccount), Headers: amqp.Table{
			"x-source": "imap", "x-namespace": event.Namespace, "x-mailbox": event.Mailbox, "x-mailbox-uid": event.MailboxUID,
		}}
}

func TestMailDeliveryRoutesAndAdvancesCursor(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mailbox := &agentsv1alpha1.Mailbox{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team", UID: types.UID("mailbox-uid")}, Spec: agentsv1alpha1.MailboxSpec{Address: "support@example.test", IMAP: &agentsv1alpha1.MailIMAP{Folder: "INBOX"}}}
	agent := &agentsv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team", UID: types.UID("agent-uid")}}
	task := &agentsv1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "thread", Namespace: "team", UID: types.UID("task-uid"), Labels: map[string]string{"codemowers.ai/agent": "support"}, OwnerReferences: []metav1.OwnerReference{{APIVersion: agentsv1alpha1.GroupVersion.String(), Kind: "Agent", Name: "support", UID: agent.UID}}}, Spec: agentsv1alpha1.TaskSpec{AgentRef: "support", RoutingKeys: []string{"mid:root@example.test"}}}
	binding := &agentsv1alpha1.MailBinding{ObjectMeta: metav1.ObjectMeta{Name: "support", Namespace: "team", UID: types.UID("binding-uid")}, Spec: agentsv1alpha1.MailBindingSpec{MailboxRef: "support", AgentRef: "support", Match: agentsv1alpha1.MailMatch{FromDomain: "example.test", Headers: []agentsv1alpha1.MailHeaderRule{{Name: "Message-ID", Pattern: `^<[^<>]+>$`}}}, Event: agentsv1alpha1.MailEvent{Type: "mail_message", Actor: "${mail.fromAddress}", Text: "New mail", Key: "mid:${mail.threadRootMessageID}"}}}
	k8s := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(mailbox, task).WithObjects(mailbox, agent, task, binding).Build()
	broker := &coalesceBroker{}
	consumer := &EventConsumer{Client: k8s, APIReader: k8s, AMQP: broker}
	event := mailDelivery{Version: 1, Namespace: "team", Mailbox: "support", MailboxUID: "mailbox-uid", Folder: "INBOX", UIDValidity: 12, UID: 7, ThreadID: "server-thread-1", Headers: map[string][]string{"from": {"Person <person@example.test>"}, "message-id": {"<reply@example.test>"}, "references": {"<root@example.test>"}}}
	// A Mailbox Pod of another namespace cannot publish this namespace's mail.
	foreign := mailboxDelivery(t, event)
	foreign.UserId = agentsv1alpha1.ServiceAccountUser("other", MailboxServiceAccount)
	if err := consumer.handle(context.Background(), foreign); err != nil || len(broker.messages) != 0 {
		t.Fatalf("mail from another namespace's Mailbox routed: %v %+v", err, broker.messages)
	}
	delivery := mailboxDelivery(t, event)
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 || len(task.Status.Conversation) != 1 || task.Status.Conversation[0].Source != "imap" {
		t.Fatalf("mail did not route: task=%+v broker=%+v", task.Status, broker.messages)
	}
	if len(task.Status.ExternalReferences) != 1 || task.Status.ExternalReferences[0].Source != "imap" ||
		task.Status.ExternalReferences[0].ID != "<root@example.test>" ||
		task.Status.ExternalReferences[0].MailboxUID != "mailbox-uid" ||
		task.Status.ExternalReferences[0].IMAPThreadID != "server-thread-1" {
		t.Fatalf("mail thread reference missing: %+v", task.Status.ExternalReferences)
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(mailbox), mailbox); err != nil {
		t.Fatal(err)
	}
	if mailbox.Status.IMAP == nil || mailbox.Status.IMAP.Cursor == nil || mailbox.Status.IMAP.Cursor.LastUID != 7 {
		t.Fatalf("cursor not advanced: %+v", mailbox.Status)
	}
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 {
		t.Fatal("replay woke Task twice")
	}
	if err := k8s.Get(context.Background(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if len(task.Status.ExternalReferences) != 1 {
		t.Fatalf("replay duplicated thread reference: %+v", task.Status.ExternalReferences)
	}
	delivery.Headers["x-mailbox"] = "another"
	if err := consumer.handle(context.Background(), delivery); err != nil {
		t.Fatal(err)
	}
	if len(broker.messages) != 1 {
		t.Fatal("tampered envelope woke Task")
	}
}
