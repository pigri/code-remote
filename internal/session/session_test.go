package session

import "testing"

func TestGenUUID(t *testing.T) {
	a, err := genUUID()
	if err != nil {
		t.Fatalf("genUUID: %v", err)
	}
	if !uuidRe.MatchString(a) {
		t.Errorf("not a valid uuid: %q", a)
	}
	if a[14] != '4' {
		t.Errorf("expected version 4, got %q", a)
	}
	b, _ := genUUID()
	if a == b {
		t.Errorf("expected distinct uuids, got %q twice", a)
	}
}

func TestValidID(t *testing.T) {
	m := &Manager{Prefix: "p"}
	cases := map[string]bool{
		"6fd0b321-a454-4b40-9aed-131afe120d36":  true,
		"6FD0B321-A454-4B40-9AED-131AFE120D36":  false, // uppercase not allowed
		"not-a-uuid":                            false,
		"":                                      false,
		"6fd0b321a4544b409aed131afe120d36":      false, // no dashes
		"../etc/passwd":                         false,
		"6fd0b321-a454-4b40-9aed-131afe120d36 ": false, // trailing space
	}
	for in, want := range cases {
		if got := m.ValidID(in); got != want {
			t.Errorf("ValidID(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestScreenName(t *testing.T) {
	m := &Manager{Prefix: "pigri-dev-remote"}
	id := "6fd0b321-a454-4b40-9aed-131afe120d36"
	if got, want := m.screenName(id), "pigri-dev-remote-"+id; got != want {
		t.Errorf("screenName = %q, want %q", got, want)
	}
}

func TestParseSessions(t *testing.T) {
	m := &Manager{Prefix: "test-rc"} // ClaudeHome empty -> Title stays ""
	out := "There are screens on:\n" +
		"\t12345.test-rc-6fd0b321-a454-4b40-9aed-131afe120d36\t(06/15/2026 08:41:26 AM)\t(Detached)\n" +
		"\t12346.test-rc-a9c1cf1e-ce20-4833-9eeb-7acf5c327506\t(06/15/2026 09:00:00 AM)\t(Attached)\n" +
		"\t99999.some-other-session\t(06/15/2026 09:00:00 AM)\t(Detached)\n" + // foreign: no prefix
		"\t77777.test-rc-not-a-uuid\t(06/15/2026 09:00:00 AM)\t(Detached)\n" + // prefix but bad id
		"4 Sockets in /run/screen/S-pigri.\n"

	got := m.parseSessions(out)
	if len(got) != 2 {
		t.Fatalf("expected 2 sessions, got %d: %+v", len(got), got)
	}

	if got[0].ID != "6fd0b321-a454-4b40-9aed-131afe120d36" {
		t.Errorf("id[0] = %q", got[0].ID)
	}
	if got[0].PID != "12345" || got[0].Status != "Detached" {
		t.Errorf("session[0] = %+v", got[0])
	}
	if got[0].Screen != "test-rc-6fd0b321-a454-4b40-9aed-131afe120d36" {
		t.Errorf("screen[0] = %q", got[0].Screen)
	}
	if got[0].CreatedAt == "" {
		t.Errorf("expected created_at to be parsed, got empty")
	}
	if got[1].Status != "Attached" {
		t.Errorf("session[1] status = %q, want Attached", got[1].Status)
	}
}

func TestParseSessionsEmpty(t *testing.T) {
	m := &Manager{Prefix: "test-rc"}
	if got := m.parseSessions("No Sockets found in /run/screen/S-pigri.\n"); len(got) != 0 {
		t.Errorf("expected 0 sessions, got %+v", got)
	}
}

func TestParseTitle(t *testing.T) {
	id := "6fd0b321-a454-4b40-9aed-131afe120d36"
	data := []byte(
		`{"type":"custom-title","customTitle":"first","sessionId":"` + id + `"}` + "\n" +
			`{"type":"user","message":"hi"}` + "\n" +
			`{"type":"custom-title","customTitle":"second","sessionId":"` + id + `"}` + "\n" +
			`{"type":"custom-title" BROKEN JSON ...` + "\n", // malformed but contains the marker
	)
	if got := parseTitle(data); got != "second" {
		t.Errorf("parseTitle = %q, want %q (last valid wins, malformed ignored)", got, "second")
	}

	if got := parseTitle([]byte(`{"type":"user","message":"hi"}` + "\n")); got != "" {
		t.Errorf("parseTitle with no title = %q, want empty", got)
	}

	if got := parseTitle(nil); got != "" {
		t.Errorf("parseTitle(nil) = %q, want empty", got)
	}
}
