package main

import "testing"

func TestSlackRoutingKey(t *testing.T) {
	cases := map[string]string{
		`{"type":"event_callback","team_id":"T1","event_id":"E1","event":{"type":"message","channel":"C1","ts":"1.1"}}`:                   "slack:T1:C1:1.1",
		`{"type":"event_callback","team_id":"T1","event_id":"E2","event":{"type":"message","channel":"C1","ts":"2.2","thread_ts":"1.1"}}`: "slack:T1:C1:1.1",
		`{"type":"event_callback","team_id":"T1","event_id":"E3","event":{"channel_type":"im","user":"U9","channel":"D1","ts":"3"}}`:      "slack:T1:dm:U9",
		`{"type":"event_callback","team_id":"T1","event_id":"E4","event":{"bot_id":"B1","channel":"C1","ts":"4"}}`:                        "",
	}
	for body, want := range cases {
		got, _, _, err := slackRoutingKey([]byte(body))
		if err != nil || got != want {
			t.Errorf("%s: got %q err %v, want %q", body, got, err, want)
		}
	}
	_, _, ch, _ := slackRoutingKey([]byte(`{"type":"url_verification","challenge":"abc"}`))
	if ch != "abc" {
		t.Errorf("challenge %q", ch)
	}
}

func TestSplitAlerts(t *testing.T) {
	ev, err := splitAlerts([]byte(`{"groupKey":"g","alerts":[{"fingerprint":"a","status":"firing"},{"fingerprint":"b","status":"resolved"}]}`))
	if err != nil || len(ev) != 2 || ev[0].key != "alertmanager:a" || ev[1].key != "alertmanager:b" {
		t.Fatalf("%v %v", ev, err)
	}
}
