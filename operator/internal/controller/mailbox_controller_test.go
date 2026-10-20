package controller

import (
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

func TestIMAPPodProjectsOnlyInboundCredentials(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := agentsv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	mailbox := &agentsv1alpha1.Mailbox{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team", UID: "mailbox-uid"}, Spec: agentsv1alpha1.MailboxSpec{
		IMAP: &agentsv1alpha1.MailIMAP{Host: "imap.example.test", CredentialsSecretRef: agentsv1alpha1.MailSecretRef{Name: "inbound", UsernameKey: "user", PasswordKey: "pass"}},
		SMTP: &agentsv1alpha1.MailSMTP{Host: "smtp.example.test", CredentialsSecretRef: agentsv1alpha1.MailSecretRef{Name: "outbound", UsernameKey: "user", PasswordKey: "pass"}},
	}}
	r := &MailboxReconciler{Scheme: scheme, Image: "example.test/mailbox@sha256:abc", AMQPURL: "amqp://broker:5672/%2F"}
	pod, err := r.imapPod(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if pod.Spec.AutomountServiceAccountToken == nil || *pod.Spec.AutomountServiceAccountToken || pod.Spec.HostUsers == nil || *pod.Spec.HostUsers {
		t.Fatal("mailbox Pod has ambient Kubernetes privileges")
	}
	for _, env := range pod.Spec.Containers[0].Env {
		if env.ValueFrom != nil && env.ValueFrom.SecretKeyRef != nil && env.ValueFrom.SecretKeyRef.Name == "outbound" {
			t.Fatal("SMTP credential projected into IMAP Pod")
		}
	}
}

func TestMailboxPodKeepsCredentialsInSeparateContainers(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = agentsv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	mailbox := &agentsv1alpha1.Mailbox{ObjectMeta: metav1.ObjectMeta{Name: "shared", Namespace: "team", UID: "mailbox-uid"}, Spec: agentsv1alpha1.MailboxSpec{
		IMAP: &agentsv1alpha1.MailIMAP{Host: "imap.example.test", CredentialsSecretRef: agentsv1alpha1.MailSecretRef{Name: "inbound", UsernameKey: "user", PasswordKey: "pass"}},
		SMTP: &agentsv1alpha1.MailSMTP{Host: "smtp.example.test", CredentialsSecretRef: agentsv1alpha1.MailSecretRef{Name: "outbound", UsernameKey: "user", PasswordKey: "pass"}},
	}}
	r := &MailboxReconciler{Scheme: scheme, Image: "example.test/mailbox@sha256:abc", AMQPURL: "amqp://broker:5672/%2F"}
	pod, err := r.mailboxPod(mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers) != 3 || pod.Name != "agent-mailbox-shared" || pod.Spec.ServiceAccountName != MailboxServiceAccount {
		t.Fatalf("unexpected mailbox Pod: %s with %d containers", pod.Name, len(pod.Spec.Containers))
	}
	for _, container := range pod.Spec.Containers {
		for _, env := range container.Env {
			if env.ValueFrom == nil || env.ValueFrom.SecretKeyRef == nil {
				continue
			}
			secret := env.ValueFrom.SecretKeyRef.Name
			if container.Name == "imap" && secret == "outbound" || container.Name == "smtp" && secret == "inbound" || container.Name == "mcp" {
				t.Fatalf("%s container received %s Secret", container.Name, secret)
			}
		}
		// Only the MCP container may call the Kubernetes API; IMAP gets only
		// its broker token.
		for _, mount := range container.VolumeMounts {
			if mount.Name == "kube-api-access" && container.Name != "mcp" || mount.Name == "agent-rabbitmq-token" && container.Name != "imap" {
				t.Fatalf("%s container has unexpected %s mount", container.Name, mount.Name)
			}
		}
	}
}
