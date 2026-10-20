package controller

import (
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"regexp"
	"strings"

	agentsv1alpha1 "github.com/codemowers/agent/operator/api/v1alpha1"
)

var mailVariable = regexp.MustCompile(`\$\{([A-Za-z][A-Za-z0-9_.]*)\}`)
var mailMessageID = regexp.MustCompile(`<[^<>\s]{1,990}>`)

// mailMetadata contains only bounded RFC 5322 headers. Bodies and attachments
// are never supplied to subscription matchers.
type mailMetadata struct {
	Headers map[string][]string `json:"headers"`
}

type matchedMailEvent struct {
	Type  string
	Actor string
	Text  string
	URL   string
	Key   []string
}

func validateMailSubscription(spec agentsv1alpha1.MailSubscriptionSpec) error {
	known := map[string]bool{"mail.fromAddress": true, "mail.messageID": true, "mail.threadRootMessageID": true}
	for _, rule := range spec.Match.Headers {
		if rule.Pattern == "" {
			continue
		}
		if len(rule.Pattern) > 2048 {
			return errors.New("mail regex too long")
		}
		pattern, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return fmt.Errorf("invalid mail regex for %s: %w", rule.Name, err)
		}
		for _, name := range pattern.SubexpNames() {
			if name == "" {
				continue
			}
			if known[name] {
				return fmt.Errorf("duplicate mail capture %q", name)
			}
			known[name] = true
		}
	}
	for _, template := range append([]string{spec.Event.Actor, spec.Event.Text, spec.Event.URL}, spec.Event.Key...) {
		for _, match := range mailVariable.FindAllStringSubmatch(template, -1) {
			if !known[match[1]] {
				return fmt.Errorf("unknown mail template variable %q", match[1])
			}
		}
		if strings.Contains(mailVariable.ReplaceAllString(template, ""), "${") {
			return errors.New("invalid mail template variable")
		}
	}
	return nil
}

func singleMailHeader(headers map[string][]string, name string) (string, bool) {
	var values []string
	for key, part := range headers {
		if strings.EqualFold(key, name) {
			values = append(values, part...)
		}
	}
	if len(values) != 1 || len(values[0]) > 4096 || strings.ContainsAny(values[0], "\r\n") {
		return "", false
	}
	return values[0], true
}

func threadRootMessageID(headers map[string][]string) string {
	for _, name := range []string{"References", "In-Reply-To", "Message-ID"} {
		value, ok := singleMailHeader(headers, name)
		if !ok {
			continue
		}
		if id := mailMessageID.FindString(value); id != "" {
			return id
		}
	}
	return ""
}

func expandMailTemplate(template string, values map[string]string) (string, error) {
	var missing string
	result := mailVariable.ReplaceAllStringFunc(template, func(token string) string {
		name := token[2 : len(token)-1]
		value := values[name]
		if value == "" {
			missing = name
		}
		return value
	})
	if missing != "" {
		return "", fmt.Errorf("missing mail template variable %q", missing)
	}
	if strings.Contains(result, "${") {
		return "", errors.New("invalid mail template variable")
	}
	return result, nil
}

// matchMailSubscription returns false for ordinary nonmatches, and an error for
// invalid rule configuration. Duplicate matched headers are deliberately a
// nonmatch: picking one could let a sender choose the apparent auth verdict.
func matchMailSubscription(spec agentsv1alpha1.MailSubscriptionSpec, message mailMetadata) (matchedMailEvent, bool, error) {
	if err := validateMailSubscription(spec); err != nil {
		return matchedMailEvent{}, false, err
	}
	from, ok := singleMailHeader(message.Headers, "From")
	if !ok {
		return matchedMailEvent{}, false, nil
	}
	address, err := mail.ParseAddress(from)
	if err != nil || address.Address == "" {
		return matchedMailEvent{}, false, nil
	}
	fromAddress := strings.ToLower(address.Address)
	parts := strings.Split(fromAddress, "@")
	if len(parts) != 2 {
		return matchedMailEvent{}, false, nil
	}
	if spec.Match.FromDomain != "" && !strings.EqualFold(parts[1], spec.Match.FromDomain) {
		return matchedMailEvent{}, false, nil
	}
	for _, excluded := range spec.Match.ExceptFromAddresses {
		if strings.EqualFold(fromAddress, excluded) {
			return matchedMailEvent{}, false, nil
		}
	}
	values := map[string]string{"mail.fromAddress": fromAddress, "mail.threadRootMessageID": threadRootMessageID(message.Headers)}
	if id, ok := singleMailHeader(message.Headers, "Message-ID"); ok && mailMessageID.MatchString(id) {
		values["mail.messageID"] = mailMessageID.FindString(id)
	}
	for _, rule := range spec.Match.Headers {
		value, present := singleMailHeader(message.Headers, rule.Name)
		if !present {
			return matchedMailEvent{}, false, nil
		}
		if rule.StartsWith != "" && !strings.HasPrefix(value, rule.StartsWith) {
			return matchedMailEvent{}, false, nil
		}
		if rule.NotEquals != "" && value == rule.NotEquals {
			return matchedMailEvent{}, false, nil
		}
		if rule.Pattern == "" {
			continue
		}
		pattern, err := regexp.Compile(rule.Pattern)
		if err != nil {
			return matchedMailEvent{}, false, err
		}
		groups := pattern.FindStringSubmatch(value)
		if groups == nil {
			return matchedMailEvent{}, false, nil
		}
		for i, name := range pattern.SubexpNames() {
			if name == "" {
				continue
			}
			if _, exists := values[name]; exists {
				return matchedMailEvent{}, false, fmt.Errorf("duplicate mail capture %q", name)
			}
			values[name] = groups[i]
		}
	}
	result := matchedMailEvent{Type: spec.Event.Type}
	if result.Actor, err = expandMailTemplate(spec.Event.Actor, values); err != nil {
		return matchedMailEvent{}, false, err
	}
	if result.Text, err = expandMailTemplate(spec.Event.Text, values); err != nil {
		return matchedMailEvent{}, false, err
	}
	if result.URL, err = expandMailTemplate(spec.Event.URL, values); err != nil {
		return matchedMailEvent{}, false, err
	}
	if result.URL != "" {
		parsed, err := url.Parse(result.URL)
		if err != nil || len(result.URL) > 2048 || parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil || strings.ContainsAny(result.URL, "\r\n") {
			return matchedMailEvent{}, false, errors.New("invalid mail event URL")
		}
	}
	result.Key = []string{"external"}
	for _, template := range spec.Event.Key {
		segment, err := expandMailTemplate(template, values)
		if err != nil {
			return matchedMailEvent{}, false, err
		}
		if segment == "" || len(segment) > 256 {
			return matchedMailEvent{}, false, errors.New("mail key segment invalid")
		}
		result.Key = append(result.Key, segment)
	}
	if len(result.Key) > 16 || len(result.Text) > 4096 {
		return matchedMailEvent{}, false, errors.New("mail event exceeds bounds")
	}
	return result, true, nil
}
