package contract

import (
	"strings"
	"testing"
)

func rowsOf(t *testing.T, jsons ...string) []Value {
	t.Helper()
	out := make([]Value, len(jsons))
	for i, j := range jsons {
		v, err := ParseJSON([]byte(j))
		if err != nil {
			t.Fatal(err)
		}
		out[i] = v
	}
	return out
}

func chatTables(t *testing.T, texts, deliveries []string) []Table {
	tables := []Table{
		{Name: "chat_message_texts", Rows: rowsOf(t, texts...)},
		{Name: "chat_messages", Rows: rowsOf(t, deliveries...)},
	}
	linkChatDeliveries(tables)
	return tables
}

var idsLetGo = []KnownDifference{
	{Where: "table chat_message_texts row *.id", Volatile: true, Reason: "race"},
	{Where: "table chat_messages row *.id", Volatile: true, Reason: "race"},
	{Where: "table chat_messages row *.message_text_id", Volatile: true, Reason: "race"},
}

func TestLinkChatDeliveries(t *testing.T) {
	// Node: the event (to system, 1) is text 101, the notice (to agent 3)
	// text 102. Texts are already in content order, as the snapshot reads
	// them.
	node := Result{Tables: chatTables(t,
		[]string{`{"id":102,"message":"Discussion #5 was concluded","from_actor_id":1,"discussion_id":null}`,
			`{"id":101,"message":"Discussion concluded","from_actor_id":1,"discussion_id":5}`},
		[]string{`{"id":1,"message_text_id":102,"to_actor_id":3}`, `{"id":2,"message_text_id":101,"to_actor_id":1}`})}

	// Go wrote the same rows in the other order: every id differs, every
	// link still points at the same content. That passes.
	renumbered := Result{Tables: chatTables(t,
		[]string{`{"id":101,"message":"Discussion #5 was concluded","from_actor_id":1,"discussion_id":null}`,
			`{"id":102,"message":"Discussion concluded","from_actor_id":1,"discussion_id":5}`},
		[]string{`{"id":2,"message_text_id":101,"to_actor_id":3}`, `{"id":1,"message_text_id":102,"to_actor_id":1}`})}
	rep, err := Compare(node, renumbered, idsLetGo)
	if err != nil || !rep.OK() {
		t.Fatalf("renumbered: %v %s", err, rep)
	}

	// A delivery that points at the other existing text fails, though
	// every id is let go.
	swapped := Result{Tables: chatTables(t,
		[]string{`{"id":102,"message":"Discussion #5 was concluded","from_actor_id":1,"discussion_id":null}`,
			`{"id":101,"message":"Discussion concluded","from_actor_id":1,"discussion_id":5}`},
		[]string{`{"id":1,"message_text_id":101,"to_actor_id":3}`, `{"id":2,"message_text_id":102,"to_actor_id":1}`})}
	rep, err = Compare(node, swapped, idsLetGo)
	if err != nil {
		t.Fatal(err)
	}
	if rep.OK() || !strings.Contains(rep.String(), "linked_text") {
		t.Fatalf("swapped references passed: %s", rep)
	}

	// A reference to no text links to null.
	tables := chatTables(t, nil, []string{`{"id":1,"message_text_id":9,"to_actor_id":3}`})
	if got, _ := tables[1].Rows[0].Get(linkedText); got.Kind != Null {
		t.Errorf("dangling reference: %s", got.Compact())
	}
}
