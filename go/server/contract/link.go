package contract

import (
	"sort"
	"strconv"
)

// linkedText is the member linkChatDeliveries adds to each delivery.
const linkedText = "linked_text"

// linkChatDeliveries gives each chat_messages row the text it points at
// (the message, sender and discussion of its chat_message_texts row, or
// null when there is none) as a linked_text member, then orders the rows by
// recipient, that text and id. A discussion conclude starts two message
// chains without awaiting either, and which one Node inserts first is a race
// between commits; those cases let the ids go (volatile) but the link from a
// delivery to the content it delivers is still compared.
func linkChatDeliveries(tables []Table) {
	texts := map[string]Value{}
	var deliveries *Table
	for i := range tables {
		switch tables[i].Name {
		case "chat_message_texts":
			for _, row := range tables[i].Rows {
				id, _ := row.Get("id")
				texts[id.Num] = textContent(row)
			}
		case "chat_messages":
			deliveries = &tables[i]
		}
	}
	if deliveries == nil {
		return
	}
	type keyed struct {
		row    Value
		to, id int64
		t      string
	}
	rows := make([]keyed, len(deliveries.Rows))
	for i, row := range deliveries.Rows {
		ref, _ := row.Get("message_text_id")
		text, ok := texts[ref.Num]
		if !ok || ref.Kind != Number {
			text = Value{Kind: Null}
		}
		row.Members = append(row.Members, Member{Key: linkedText, Value: text})
		to, _ := row.Get("to_actor_id")
		id, _ := row.Get("id")
		k := keyed{row: row, t: text.Compact()}
		k.to, _ = strconv.ParseInt(to.Num, 10, 64)
		k.id, _ = strconv.ParseInt(id.Num, 10, 64)
		rows[i] = k
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if a.to != b.to {
			return a.to < b.to
		}
		if a.t != b.t {
			return a.t < b.t
		}
		return a.id < b.id
	})
	for i, k := range rows {
		deliveries.Rows[i] = k.row
	}
}

// textContent is what a delivery delivers: its text row without the id.
func textContent(row Value) Value {
	out := Value{Kind: Object}
	for _, key := range []string{"message", "from_actor_id", "discussion_id"} {
		if v, ok := row.Get(key); ok {
			out.Members = append(out.Members, Member{Key: key, Value: v})
		}
	}
	return out
}
