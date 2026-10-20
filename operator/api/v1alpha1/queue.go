package v1alpha1

// RabbitMQAudience is the projected token audience workers present to the
// broker, which also maps it to their queue scopes.
const RabbitMQAudience = "agent-rabbitmq"

// RabbitMQPublisherAudience is the projected token audience of event
// publishers, which may only write to the intake exchange. RabbitMQ verifies
// the user_id they stamp on each event against their token subject.
const RabbitMQPublisherAudience = "agent-rabbitmq-publisher"

// ServiceAccountUser is the broker username of a ServiceAccount token.
func ServiceAccountUser(namespace, name string) string {
	return "system:serviceaccount:" + namespace + ":" + name
}

// WorkerServiceAccount names the ServiceAccount of the Agent's worker Pods.
func WorkerServiceAccount(agent *Agent) string {
	if agent.Spec.Template.Spec.ServiceAccountName != "" {
		return agent.Spec.Template.Spec.ServiceAccountName
	}
	return "agent-worker-" + agent.Name
}

// TaskQueue names the Task's RabbitMQ queue after the worker token subject.
// The broker grants each token `{sub}:*`, so workers read only queues of
// their own ServiceAccount. Kubernetes names cannot contain ':', and the
// broker splits scope patterns on '/', so neither can be the separator.
func TaskQueue(task *Task, agent *Agent) string {
	return ServiceAccountUser(task.Namespace, WorkerServiceAccount(agent)) + ":" + task.Name
}
