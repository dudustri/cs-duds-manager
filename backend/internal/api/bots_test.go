package api_test

import "testing"

// Many bots at once: a count, and "both" to fill the two teams alternately.
func TestBotsMany(t *testing.T) {
	h := start(t)
	r := h.create(t, "de_dust2", "classic")
	add := func(body any, status int, code string) {
		t.Helper()
		h.expect(t, "POST", "/api/rooms/"+r.ID+"/bots", body, r.bearer(), status, code)
	}
	add(map[string]any{"team": "both", "difficulty": 1, "count": 6}, 204, "")
	add(map[string]any{"team": "ct", "difficulty": 1, "count": 16}, 204, "")
	for _, n := range []int{-1, 17} {
		add(map[string]any{"team": "both", "difficulty": 1, "count": n}, 400, "bad_request")
	}
}
