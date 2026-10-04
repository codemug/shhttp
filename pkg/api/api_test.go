package api

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestEventJSON(t *testing.T) {
	text := Event{Seq: 1, Type: EventStdout, Data: []byte("a < b && c > d\n")}
	b, err := Marshal(text)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"data":"a < b && c > d\n"`) {
		t.Fatalf("text event = %s", b)
	}
	bin := Event{Seq: 2, Type: EventStdout, Data: []byte{0xff, 0x00}}
	b, _ = Marshal(bin)
	if !strings.Contains(string(b), `"data_b64":"/wA="`) || strings.Contains(string(b), `"data":`) {
		t.Fatalf("binary event = %s", b)
	}
	var back Event
	if err := json.Unmarshal(b, &back); err != nil || string(back.Data) != "\xff\x00" || back.Seq != 2 {
		t.Fatalf("round trip = %+v, %v", back, err)
	}
	// Events without data omit both fields.
	b, _ = Marshal(Event{Seq: 3, Type: EventStarted, PID: 7})
	if strings.Contains(string(b), "data") {
		t.Fatalf("started event = %s", b)
	}
}

func TestDuration(t *testing.T) {
	var d Duration
	if err := json.Unmarshal([]byte(`"1m30s"`), &d); err != nil || d.Std() != 90*time.Second {
		t.Fatalf("%v, %v", d, err)
	}
	for _, bad := range []string{`90`, `"soon"`, `"-1s"`} {
		if err := json.Unmarshal([]byte(bad), &d); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	b, _ := json.Marshal(Duration(2 * time.Second))
	if string(b) != `"2s"` {
		t.Fatalf("marshal = %s", b)
	}
}
