package ytdlp

import "testing"

func TestParseTarget(t *testing.T) {
	good := map[string]string{
		"dQw4w9WgXcQ": "dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=-dQw4w9WgXc":        "-dQw4w9WgXc", // IDs may start with '-'; always passed after "--"
		"https://www.youtube.com/watch?v=dQw4w9WgXcQ&list=x": "dQw4w9WgXcQ",
		"https://music.youtube.com/watch?v=dQw4w9WgXcQ":      "dQw4w9WgXcQ",
		"https://youtu.be/dQw4w9WgXcQ?t=3":                   "dQw4w9WgXcQ",
		"https://www.youtube.com/shorts/dQw4w9WgXcQ":         "dQw4w9WgXcQ",
		"  https://m.youtube.com/embed/dQw4w9WgXcQ/extra  ":  "dQw4w9WgXcQ",
	}
	for in, want := range good {
		got, ok := ParseTarget(in)
		if !ok || got != want {
			t.Errorf("ParseTarget(%q) = %q,%v; want %q", in, got, ok, want)
		}
	}
	bad := []string{
		"", "short", "dQw4w9WgXcQ; rm -rf /", "--exec=id", "file:///etc/passwd",
		"http://169.254.169.254/latest/meta-data",
		"https://evil.example/watch?v=dQw4w9WgXcQ",
		"https://youtube.com.evil.example/watch?v=dQw4w9WgXcQ",
		"https://www.youtube.com/watch?v=../../etc/passwd",
		"https://www.youtube.com/playlist?list=PL123",
	}
	for _, in := range bad {
		if id, ok := ParseTarget(in); ok {
			t.Errorf("ParseTarget(%q) accepted as %q", in, id)
		}
	}
}

func TestParseEntriesSkipsNonVideos(t *testing.T) {
	raw := []byte(`{"entries":[
		{"id":"dQw4w9WgXcQ","title":"Song","channel":"Artist - Topic","duration":212.5},
		{"id":"UCxxxxxxxxxxxxxxxxxxxxxx","title":"A channel"},
		{"id":"abc","title":"too short"},
		{"id":"aaaaaaaaaaa","title":""}]}`)
	got, err := parseEntries(raw)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %v, %v", got, err)
	}
	if got[0].Artist != "Artist" || got[0].Duration != 212 {
		t.Fatalf("unexpected entry %+v", got[0])
	}
}

func TestClassifyExplainsWhatFailed(t *testing.T) {
	cases := map[string]Kind{
		"ERROR: [youtube] x: Sign in to confirm you’re not a bot":                                                         KindBlocked,
		"ERROR: [youtube] x: Video unavailable":                                                                           KindUnavailable,
		"ERROR: [youtube] x: Private video. Sign in if you've been granted":                                               KindUnavailable,
		"ERROR: unable to download video data: HTTP Error 429: Too Many Requests":                                         KindRateLimited,
		"ERROR: [youtube] x: Unable to download webpage: <urlopen error [Errno -3] Temporary failure in name resolution>": KindNetwork,
		"WARNING: No supported JavaScript runtime could be found\nERROR: n challenge solving failed":                      KindJSRuntime,
		"ERROR: [youtube] x: Requested format is not available":                                                           KindOutdated,
		"ERROR: This video may be inappropriate for some users. Sign in to confirm your age":                              KindAgeRestrict,
		"something nobody has ever seen":                                                                                  KindFailed,
	}
	for stderr, want := range cases {
		got := classify(stderr)
		if got.Kind != want {
			t.Errorf("classify(%q) = %s, want %s", stderr, got.Kind, want)
		}
		if got.Msg == "" || got.Detail == "" {
			t.Errorf("classify(%q) lost its message or detail: %+v", stderr, got)
		}
	}
}
