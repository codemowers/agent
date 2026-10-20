package controller

import (
	"reflect"
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

func TestGitHubMailSubscriptionMatchesTrustedHeaders(t *testing.T) {
	spec := agentsv1alpha1.MailSubscriptionSpec{
		Match: agentsv1alpha1.MailMatch{FromDomain: "github.com", Headers: []agentsv1alpha1.MailHeaderRule{
			{Name: "Authentication-Results", StartsWith: "mail.codemowers.io;", Pattern: `(?:^|;) *dmarc=pass(?:[ (;]|$)`},
			{Name: "Message-ID", Pattern: `^<(?P<owner>[^/<>]+)/(?P<repository>[^/<>]+)/issues?/(?P<number>[0-9]+)(?:/(?:[0-9]+|issue_event/[0-9]+))?@github[.]com>$`},
			{Name: "X-GitHub-Sender", Pattern: `^(?P<actor>[A-Za-z0-9-]+)$`, NotEquals: "matmlazi"},
		}},
		Event: agentsv1alpha1.MailEvent{Type: "github_notification", Actor: "${actor}", Text: "Read issue", Key: []string{"forge", "github", "https://github.com", "${owner}", "${repository}", "thread", "${number}"}},
	}
	mail := mailMetadata{Headers: map[string][]string{
		"From":                   {"Mat Mlazi <notifications@github.com>"},
		"Authentication-Results": {"mail.codemowers.io; spf=pass; dkim=pass; dmarc=pass header.from=github.com"},
		"Message-ID":             {"<codemowers/agent/issues/8/6189219592@github.com>"},
		"X-GitHub-Sender":        {"laurivosandi"},
	}}
	got, matched, err := matchMailSubscription(spec, mail)
	if err != nil || !matched || got.Actor != "laurivosandi" || !reflect.DeepEqual(got.Key, []string{"external", "forge", "github", "https://github.com", "codemowers", "agent", "thread", "8"}) {
		t.Fatalf("match=%v event=%+v err=%v", matched, got, err)
	}
	mail.Headers["Authentication-Results"] = []string{"evil.example; dmarc=pass header.from=github.com"}
	if _, matched, err := matchMailSubscription(spec, mail); err != nil || matched {
		t.Fatal("untrusted authentication result matched")
	}
	mail.Headers["Authentication-Results"] = []string{"mail.codemowers.io; dmarc=pass", "mail.codemowers.io; dmarc=pass"}
	if _, matched, err := matchMailSubscription(spec, mail); err != nil || matched {
		t.Fatal("ambiguous authentication result matched")
	}
}

func TestMailThreadRootAndTemplateValidation(t *testing.T) {
	headers := map[string][]string{"References": {"<first@example.test> <later@example.test>"}, "In-Reply-To": {"<reply@example.test>"}, "Message-ID": {"<current@example.test>"}}
	if got := threadRootMessageID(headers); got != "<first@example.test>" {
		t.Fatalf("thread root %q", got)
	}
	if _, err := expandMailTemplate("${unknown}", map[string]string{}); err == nil {
		t.Fatal("unknown template variable accepted")
	}
	spec := agentsv1alpha1.MailSubscriptionSpec{Match: agentsv1alpha1.MailMatch{Headers: []agentsv1alpha1.MailHeaderRule{{Name: "Message-ID", Pattern: `(?P<id>.+)`}, {Name: "Subject", Pattern: `(?P<id>.+)`}}}, Event: agentsv1alpha1.MailEvent{Type: "thread", Actor: "${id}", Text: "text", Key: []string{"${id}"}}}
	if _, _, err := matchMailSubscription(spec, mailMetadata{Headers: map[string][]string{"From": {"sender@example.test"}, "Message-ID": {"<one@example.test>"}, "Subject": {"hello"}}}); err == nil {
		t.Fatal("duplicate capture accepted")
	}
}

func TestGitHubWebhookTaskClaimsMailThreadAlias(t *testing.T) {
	issue := forgeRoutingKeys(parsedEvent{source: "github"}, []string{"external", "forge", "github", "https://github.com", "codemowers", "agent", "issue", "8"})
	pull := forgeRoutingKeys(parsedEvent{source: "github"}, []string{"external", "forge", "github", "https://github.com", "codemowers", "agent", "pull_request", "8"})
	if len(issue) != 2 || len(pull) != 2 || !reflect.DeepEqual(issue[1], pull[1]) {
		t.Fatalf("mail thread alias mismatch: issue=%v pull=%v", issue, pull)
	}
}
