package service

import "testing"

func TestNormalizeRSSPlainText(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{name: "numeric entity", input: "Episode &#8211; title", want: "Episode – title"},
		{name: "double escaped numeric entity", input: "Episode &amp;#8211; title", want: "Episode – title"},
		{name: "unicode is preserved", input: "Pokémon – episódio", want: "Pokémon – episódio"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := normalizeRSSPlainText(test.input); got != test.want {
				t.Fatalf("normalizeRSSPlainText(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}
