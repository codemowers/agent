package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type MailboxReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Image  string
	// AMQPURL is the broker address without credentials; the IMAP container
	// logs in with a projected publisher token.
	AMQPURL string
}

// MailboxServiceAccount runs Mailbox Pods. Namespace administrators bind it
// to TokenReview and SubjectAccessReview creation; only the mcp container
// receives its token.
const MailboxServiceAccount = "agent-mailbox"

func mailboxPodName(mailbox *agentsv1alpha1.Mailbox) string {
	return agentsv1alpha1.MailboxResourceName(mailbox.Name)
}

func mailboxConfigHash(mailbox *agentsv1alpha1.Mailbox, image, amqpURL string) string {
	data, _ := json.Marshal(struct {
		Spec    agentsv1alpha1.MailboxSpec
		Image   string
		AMQPURL string
	}{mailbox.Spec, image, amqpURL})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func secretEnv(name, secret, key string) corev1.EnvVar {
	return corev1.EnvVar{Name: name, ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: secret}, Key: key}}}
}

func (r *MailboxReconciler) imapPod(mailbox *agentsv1alpha1.Mailbox) (*corev1.Pod, error) {
	if r.Image == "" || r.AMQPURL == "" {
		return nil, fmt.Errorf("mailbox image and RabbitMQ URL must be configured")
	}
	config := mailbox.Spec.IMAP
	if config == nil {
		return nil, nil
	}
	port := config.Port
	if port == 0 {
		port = 993
	}
	folder := config.Folder
	if folder == "" {
		folder = "INBOX"
	}
	tls := config.TLS
	if tls == "" {
		tls = "auto"
	}
	cursorValidity, cursorUID := int64(0), int64(0)
	if mailbox.Status.IMAP != nil && mailbox.Status.IMAP.Cursor != nil && mailbox.Status.IMAP.Cursor.Folder == folder {
		cursorValidity = mailbox.Status.IMAP.Cursor.UIDValidity
		cursorUID = mailbox.Status.IMAP.Cursor.LastUID
	}
	env := []corev1.EnvVar{
		{Name: "AGENT_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "MAILBOX_NAME", Value: mailbox.Name}, {Name: "MAILBOX_UID", Value: string(mailbox.UID)},
		{Name: "IMAP_HOST", Value: config.Host}, {Name: "IMAP_PORT", Value: strconv.Itoa(int(port))},
		{Name: "IMAP_TLS", Value: tls}, {Name: "IMAP_FOLDER", Value: folder},
		{Name: "IMAP_SENT_FOLDER", Value: config.SentFolder},
		{Name: "IMAP_ARCHIVE_ENABLED", Value: strconv.FormatBool(mailbox.Spec.SMTP != nil && mailbox.Spec.SMTP.SentCopy != nil && mailbox.Spec.SMTP.SentCopy.Enabled)},
		{Name: "MAIL_CURSOR_UIDVALIDITY", Value: strconv.FormatInt(cursorValidity, 10)},
		{Name: "MAIL_CURSOR_LAST_UID", Value: strconv.FormatInt(cursorUID, 10)},
		secretEnv("IMAP_USERNAME", config.CredentialsSecretRef.Name, config.CredentialsSecretRef.UsernameKey),
		secretEnv("IMAP_PASSWORD", config.CredentialsSecretRef.Name, config.CredentialsSecretRef.PasswordKey),
		{Name: "AMQP_URL", Value: r.AMQPURL},
		{Name: "AMQP_USER_ID", Value: agentsv1alpha1.ServiceAccountUser(mailbox.Namespace, MailboxServiceAccount)},
		{Name: "AGENT_RABBITMQ_TOKEN_FILE", Value: "/var/run/agent-rabbitmq/token"},
	}
	no := false
	uid := int64(1000)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: mailboxPodName(mailbox), Namespace: mailbox.Namespace,
		Labels:      map[string]string{"app": "agent-mailbox", "codemowers.cloud/mailbox": mailbox.Name},
		Annotations: map[string]string{"codemowers.cloud/mailbox-config-hash": mailboxConfigHash(mailbox, r.Image, r.AMQPURL)},
	}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: &no,
		RestartPolicy:                corev1.RestartPolicyAlways,
		HostUsers:                    &no,
		SecurityContext:              &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, RunAsNonRoot: boolPtr(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers: []corev1.Container{{Name: "imap", Image: r.Image, ImagePullPolicy: corev1.PullAlways, Env: env,
			VolumeMounts:    []corev1.VolumeMount{{Name: "agent-rabbitmq-token", MountPath: "/var/run/agent-rabbitmq", ReadOnly: true}},
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
		}},
		Volumes: []corev1.Volume{{Name: "agent-rabbitmq-token", VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{
				Audience: agentsv1alpha1.RabbitMQPublisherAudience, ExpirationSeconds: int64Ptr(3600), Path: "token"}}}}}}},
	}}
	if err := controllerutil.SetControllerReference(mailbox, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

func (r *MailboxReconciler) smtpPod(mailbox *agentsv1alpha1.Mailbox) (*corev1.Pod, error) {
	if r.Image == "" {
		return nil, fmt.Errorf("mailbox image must be configured")
	}
	config := mailbox.Spec.SMTP
	if config == nil {
		return nil, nil
	}
	port := config.Port
	if port == 0 {
		port = 465
	}
	tls := config.TLS
	if tls == "" {
		tls = "auto"
	}
	from := config.FromAddress
	if from == "" {
		from = mailbox.Spec.Address
	}
	replyTo := config.ReplyTo
	if replyTo == "" {
		replyTo = from
	}
	no := false
	uid := int64(1000)
	env := []corev1.EnvVar{
		{Name: "AGENT_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
		{Name: "POD_NAME", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}}},
		{Name: "POD_UID", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.uid"}}},
		{Name: "MAILBOX_NAME", Value: mailbox.Name}, {Name: "MAILBOX_UID", Value: string(mailbox.UID)},
		{Name: "SMTP_HOST", Value: config.Host}, {Name: "SMTP_PORT", Value: strconv.Itoa(int(port))},
		{Name: "SMTP_TLS", Value: tls}, {Name: "SMTP_FROM_ADDRESS", Value: from},
		{Name: "SMTP_FROM_NAME", Value: mailbox.Spec.DisplayName}, {Name: "SMTP_REPLY_TO", Value: replyTo},
		{Name: "SMTP_SAVE_SENT", Value: strconv.FormatBool(config.SentCopy != nil && config.SentCopy.Enabled)},
		secretEnv("SMTP_USERNAME", config.CredentialsSecretRef.Name, config.CredentialsSecretRef.UsernameKey),
		secretEnv("SMTP_PASSWORD", config.CredentialsSecretRef.Name, config.CredentialsSecretRef.PasswordKey),
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: mailboxPodName(mailbox), Namespace: mailbox.Namespace,
		Labels:      map[string]string{"app": "agent-mailbox", "codemowers.cloud/mailbox": mailbox.Name},
		Annotations: map[string]string{"codemowers.cloud/mailbox-config-hash": mailboxConfigHash(mailbox, r.Image, r.AMQPURL)},
	}, Spec: corev1.PodSpec{
		AutomountServiceAccountToken: &no,
		RestartPolicy:                corev1.RestartPolicyAlways, HostUsers: &no,
		SecurityContext: &corev1.PodSecurityContext{RunAsUser: &uid, RunAsGroup: &uid, RunAsNonRoot: boolPtr(true), SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}},
		Containers: []corev1.Container{{Name: "smtp", Image: r.Image, ImagePullPolicy: corev1.PullAlways,
			Command: []string{"node", "/app/mailbox/smtp.js"}, Env: env,
			SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
			Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m"), corev1.ResourceMemory: resource.MustParse("256Mi")}},
		}},
	}}
	if err := controllerutil.SetControllerReference(mailbox, pod, r.Scheme); err != nil {
		return nil, err
	}
	return pod, nil
}

func (r *MailboxReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var mailbox agentsv1alpha1.Mailbox
	if err := r.Get(ctx, req.NamespacedName, &mailbox); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	return r.ensurePod(ctx, &mailbox)
}

func (r *MailboxReconciler) mailboxPod(mailbox *agentsv1alpha1.Mailbox) (*corev1.Pod, error) {
	var pod *corev1.Pod
	if mailbox.Spec.IMAP != nil {
		imap, err := r.imapPod(mailbox)
		if err != nil {
			return nil, err
		}
		pod = imap
	}
	if mailbox.Spec.SMTP != nil {
		smtp, err := r.smtpPod(mailbox)
		if err != nil {
			return nil, err
		}
		if pod == nil {
			pod = smtp
		} else {
			pod.Spec.Containers = append(pod.Spec.Containers, smtp.Spec.Containers...)
		}
	}
	if pod != nil {
		pod.Spec.ServiceAccountName = MailboxServiceAccount
		pod.Spec.Containers = append(pod.Spec.Containers, r.mcpContainer(mailbox))
		// The token reaches only the mcp container, unlike automounting.
		pod.Spec.Volumes = append(pod.Spec.Volumes, corev1.Volume{Name: "kube-api-access", VolumeSource: corev1.VolumeSource{
			Projected: &corev1.ProjectedVolumeSource{Sources: []corev1.VolumeProjection{
				{ServiceAccountToken: &corev1.ServiceAccountTokenProjection{ExpirationSeconds: int64Ptr(3600), Path: "token"}},
				{ConfigMap: &corev1.ConfigMapProjection{LocalObjectReference: corev1.LocalObjectReference{Name: "kube-root-ca.crt"},
					Items: []corev1.KeyToPath{{Key: "ca.crt", Path: "ca.crt"}}}},
			}}}})
	}
	return pod, nil
}

// mcpContainer serves the Mailbox MCP to workers. It forwards authorized
// calls to the IMAP and SMTP containers over loopback and holds no mail
// credential.
func (r *MailboxReconciler) mcpContainer(mailbox *agentsv1alpha1.Mailbox) corev1.Container {
	no := false
	return corev1.Container{Name: "mcp", Image: r.Image, ImagePullPolicy: corev1.PullAlways,
		Command: []string{"node", "/app/mailbox/mcp.js"},
		Env: []corev1.EnvVar{
			{Name: "AGENT_NAMESPACE", ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"}}},
			{Name: "MAILBOX_NAME", Value: mailbox.Name}, {Name: "MAILBOX_ADDRESS", Value: mailbox.Spec.Address},
			{Name: "MAILBOX_DISPLAY_NAME", Value: mailbox.Spec.DisplayName},
			{Name: "MAILBOX_IMAP", Value: strconv.FormatBool(mailbox.Spec.IMAP != nil)},
			{Name: "MAILBOX_SMTP", Value: strconv.FormatBool(mailbox.Spec.SMTP != nil)},
		},
		Ports:           []corev1.ContainerPort{{Name: "mcp", ContainerPort: 8080}},
		VolumeMounts:    []corev1.VolumeMount{{Name: "kube-api-access", MountPath: "/var/run/secrets/kubernetes.io/serviceaccount", ReadOnly: true}},
		SecurityContext: &corev1.SecurityContext{AllowPrivilegeEscalation: &no, ReadOnlyRootFilesystem: boolPtr(true), Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("10m"), corev1.ResourceMemory: resource.MustParse("48Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("128Mi")}},
	}
}

func (r *MailboxReconciler) ensureService(ctx context.Context, mailbox *agentsv1alpha1.Mailbox) error {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: agentsv1alpha1.MailboxResourceName(mailbox.Name), Namespace: mailbox.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.CreationTimestamp.IsZero() || metav1.IsControlledBy(service, mailbox) {
			service.Labels = map[string]string{"app": "agent-mailbox", "codemowers.cloud/mailbox": mailbox.Name}
			service.Spec.Selector = map[string]string{"app": "agent-mailbox", "codemowers.cloud/mailbox": mailbox.Name}
			service.Spec.Ports = []corev1.ServicePort{{Name: "mcp", Port: 8080, TargetPort: intstr.FromString("mcp"), Protocol: corev1.ProtocolTCP}}
			return controllerutil.SetControllerReference(mailbox, service, r.Scheme)
		}
		return fmt.Errorf("mailbox Service %s/%s has another owner", service.Namespace, service.Name)
	})
	return err
}

func (r *MailboxReconciler) ensurePod(ctx context.Context, mailbox *agentsv1alpha1.Mailbox) (ctrl.Result, error) {
	name := mailboxPodName(mailbox)
	var existing corev1.Pod
	err := r.Get(ctx, client.ObjectKey{Namespace: mailbox.Namespace, Name: name}, &existing)
	configured := mailbox.Spec.IMAP != nil || mailbox.Spec.SMTP != nil
	if !configured {
		if err == nil && metav1.IsControlledBy(&existing, mailbox) {
			return ctrl.Result{}, r.Delete(ctx, &existing)
		}
		if err != nil && !apierrors.IsNotFound(err) {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}
	if err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, err
	}
	desired, err := r.mailboxPod(mailbox)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := r.ensureService(ctx, mailbox); err != nil {
		return ctrl.Result{}, err
	}
	if existing.Name == "" {
		return ctrl.Result{}, r.Create(ctx, desired)
	}
	if !metav1.IsControlledBy(&existing, mailbox) {
		return ctrl.Result{}, fmt.Errorf("mailbox Pod %s/%s has another owner", mailbox.Namespace, name)
	}
	if existing.Annotations["codemowers.cloud/mailbox-config-hash"] != desired.Annotations["codemowers.cloud/mailbox-config-hash"] || existing.Status.Phase == corev1.PodFailed || existing.Status.Phase == corev1.PodSucceeded {
		if existing.DeletionTimestamp == nil {
			return ctrl.Result{RequeueAfter: 2 * time.Second}, r.Delete(ctx, &existing)
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

func (r *MailboxReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).For(&agentsv1alpha1.Mailbox{}).Owns(&corev1.Pod{}).Owns(&corev1.Service{}).Complete(r)
}
