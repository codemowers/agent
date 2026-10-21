package controller

import (
	"testing"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

func TestGitHubMailBindingMatchesTrustedHeaders(t *testing.T) {
	spec := agentsv1alpha1.MailBindingSpec{
		Match: agentsv1alpha1.MailMatch{FromDomain: "github.com", Headers: []agentsv1alpha1.MailHeaderRule{
			{Name: "Authentication-Results", StartsWith: "mail.codemowers.io;", Pattern: `(?:^|;) *dmarc=pass(?:[ (;]|$)`},
			{Name: "References", Pattern: `^<(?P<owner>[^/<>]+)/(?P<repository>[^/<>]+)/(?:issues|pull)/(?P<number>[0-9]+)@github[.]com>`},
			{Name: "X-GitHub-Sender", Pattern: `^(?P<actor>[A-Za-z0-9-]+)$`},
		}},
		Event: agentsv1alpha1.MailEvent{Type: "github_notification", Actor: "${actor}", Text: "Read issue", Key: "https://github.com/${owner|lower}/${repository|lower}/issues/${number}"},
	}
	mail := mailMetadata{Headers: map[string][]string{
		"From":                   {"Mat Mlazi <notifications@github.com>"},
		"Authentication-Results": {"mail.codemowers.io; spf=pass; dkim=pass; dmarc=pass header.from=github.com"},
		"References":             {"<Codemowers/Agent/pull/8@github.com> <Codemowers/Agent/pull/8/review/1@github.com>"},
		"X-GitHub-Sender":        {"laurivosandi"},
	}}
	got, matched, err := matchMailBinding(spec, mail)
	if err != nil || !matched || got.Actor != "laurivosandi" || got.Key != "https://github.com/codemowers/agent/issues/8" {
		t.Fatalf("match=%v event=%+v err=%v", matched, got, err)
	}
	mail.Headers["Authentication-Results"] = []string{"evil.example; dmarc=pass header.from=github.com"}
	if _, matched, err := matchMailBinding(spec, mail); err != nil || matched {
		t.Fatal("untrusted authentication result matched")
	}
	mail.Headers["Authentication-Results"] = []string{"mail.codemowers.io; dmarc=pass", "mail.codemowers.io; dmarc=pass"}
	if _, matched, err := matchMailBinding(spec, mail); err != nil || matched {
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
	spec := agentsv1alpha1.MailBindingSpec{Match: agentsv1alpha1.MailMatch{Headers: []agentsv1alpha1.MailHeaderRule{{Name: "Message-ID", Pattern: `(?P<id>.+)`}, {Name: "Subject", Pattern: `(?P<id>.+)`}}}, Event: agentsv1alpha1.MailEvent{Type: "thread", Actor: "${id}", Text: "text", Key: "mid:${id}"}}
	if _, _, err := matchMailBinding(spec, mailMetadata{Headers: map[string][]string{"From": {"sender@example.test"}, "Message-ID": {"<one@example.test>"}, "Subject": {"hello"}}}); err == nil {
		t.Fatal("duplicate capture accepted")
	}
}

func TestMailKeyFromInvalidCaptureIsNonmatch(t *testing.T) {
	spec := agentsv1alpha1.MailBindingSpec{Match: agentsv1alpha1.MailMatch{Headers: []agentsv1alpha1.MailHeaderRule{{Name: "Message-ID", Pattern: `^<[^<>]+>$`}}},
		Event: agentsv1alpha1.MailEvent{Type: "thread", Actor: "${mail.fromAddress}", Text: "text", Key: "mid:${mail.messageID}"}}
	got, matched, err := matchMailBinding(spec, mailMetadata{Headers: map[string][]string{"From": {"sender@example.test"}, "Message-ID": {"<one@example.test>"}}})
	if err != nil || !matched || got.Key != "mid:one@example.test" {
		t.Fatalf("bare mid key: %+v %v %v", got, matched, err)
	}
	if _, matched, err := matchMailBinding(spec, mailMetadata{Headers: map[string][]string{"From": {"sender@example.test"}, "Message-ID": {"<one?x#y@example.test>"}}}); err != nil || matched {
		t.Fatalf("invalid captured key matched=%v err=%v", matched, err)
	}
}
