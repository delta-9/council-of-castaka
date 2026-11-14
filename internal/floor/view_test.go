package floor

import (
	"reflect"
	"regexp"
	"testing"
)

// A session with three seats:
//
//	#1 chair asks; round 1 reads up to #1
//	#2 astra, #3 chair interjects mid-round, #4 opus, #5 hydrogen
//	round 2 reads up to #5: #6 opus, #7 astra fails, #8 hydrogen passes
//	#9 chair mentions @opus; round 3 reads up to #9: #10 opus
func fixture() []Entry {
	return []Entry{
		{Seq: 1, From: "chair", Kind: "msg", Text: "shard or partition?"},
		{Seq: 2, From: "astra", Kind: "msg", Text: "archive first", Seen: 1},
		{Seq: 3, From: "chair", Kind: "msg", Text: "2B rows"},
		{Seq: 4, From: "opus", Kind: "msg", Text: "partition", Seen: 1},
		{Seq: 5, From: "hydrogen", Kind: "msg", Text: "shard", Seen: 1},
		{Seq: 6, From: "opus", Kind: "msg", Text: "still partition", Seen: 5, Raw: []byte(`{"native":true}`), Model: "m"},
		{Seq: 7, From: "astra", Kind: "error", Text: "timeout", Seen: 5},
		{Seq: 8, From: "hydrogen", Kind: "pass", Text: "PASS", Seen: 5},
		{Seq: 9, From: "chair", Kind: "msg", Text: "@opus why?"},
		{Seq: 10, From: "opus", Kind: "msg", Text: "because", Seen: 9},
	}
}

var msgRe = regexp.MustCompile(`(?s)<msg from="[^"]+">\n(.*?)\n</msg>`)

// TestView checks that every message reaches each seat exactly once, in order,
// at the seat's next turn, and that the turns alternate as the APIs require.
func TestView(t *testing.T) {
	entries := fixture()
	seqOf := map[string]int{}
	for _, e := range entries {
		seqOf[e.Text] = e.Seq
	}
	cases := []struct {
		seat  string
		turns [][]int // seqs each turn carries, the queue last
		queue []int
	}{
		// #2 landed before opus's #4 but opus had only read #1: turn 2 gets it.
		{"opus", [][]int{{1}, {4}, {2, 3, 5}, {6}, {9}, {10}}, nil},
		// A failed turn leaves the queue intact; passes and errors are never delivered.
		{"astra", [][]int{{1}, {2}, {3, 4, 5, 6, 9, 10}}, []int{3, 4, 5, 6, 9, 10}},
		// A pass is the seat's own turn; the mention round reaches it next turn.
		{"hydrogen", [][]int{{1}, {5}, {2, 3, 4}, {8}, {6, 9, 10}}, []int{6, 9, 10}},
		// A seat that joins late reads the whole floor as one queue.
		{"carbon", [][]int{{1, 2, 3, 4, 5, 6, 9, 10}}, []int{1, 2, 3, 4, 5, 6, 9, 10}},
	}
	for _, c := range cases {
		turns, queue := View(entries, c.seat, 10)
		var got [][]int
		for i, turn := range turns {
			want := "user"
			if i%2 == 1 {
				want = "assistant"
			}
			if turn.Role != want {
				t.Fatalf("%s: turn %d is %s, want %s", c.seat, i, turn.Role, want)
			}
			if turn.Role == "assistant" {
				got = append(got, []int{seqOf[turn.Text]})
				continue
			}
			var seqs []int
			for _, m := range msgRe.FindAllStringSubmatch(turn.Text, -1) {
				seqs = append(seqs, seqOf[m[1]])
			}
			got = append(got, seqs)
		}
		if !reflect.DeepEqual(got, c.turns) {
			t.Errorf("%s: turns carry %v, want %v", c.seat, got, c.turns)
		}
		if !reflect.DeepEqual(queue, c.queue) {
			t.Errorf("%s: queue %v, want %v", c.seat, queue, c.queue)
		}
	}
	turns, _ := View(entries, "opus", 10)
	if string(turns[3].Raw) != `{"native":true}` || turns[3].Model != "m" {
		t.Errorf("opus's second turn lost its native reply: %+v", turns[3])
	}
}

// TestIsPass checks that markup around PASS still counts, and a sentence does not.
func TestIsPass(t *testing.T) {
	for _, s := range []string{"PASS", " pass. ", "**PASS**"} {
		if !IsPass(s) {
			t.Errorf("%q should be a pass", s)
		}
	}
	if IsPass("PASS: I agree with opus") {
		t.Error("a sentence starting with PASS is a message")
	}
}
