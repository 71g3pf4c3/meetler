package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func toggleVote(t *testing.T, vs *voteStore, kind, target, voter string) voteResponse {
	t.Helper()
	body, err := json.Marshal(voteRequest{Kind: kind, Target: target, Voter: voter})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	vs.cast(w, httptest.NewRequest(http.MethodPost, "/votes", strings.NewReader(string(body))), "test")
	if w.Code != http.StatusOK {
		t.Fatalf("vote returned %d: %s", w.Code, w.Body.String())
	}
	var result voteResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestVoteToggle(t *testing.T) {
	for kind, target := range map[string]string{"kick": "anon-test", "sound": "clip.wav", "avatar": "portrait.png", "room": "other"} {
		t.Run(kind, func(t *testing.T) {
			vs := newVoteStore(&roomService{})
			steps := []struct {
				voter string
				count int
				voted bool
			}{
				{"alice", 1, true}, {"bob", 2, true},
				{"alice", 1, false}, {"alice", 2, true},
				{"alice", 1, false}, {"bob", 0, false},
			}
			for _, step := range steps {
				result := toggleVote(t, vs, kind, target, step.voter)
				if result.Count != step.count || result.Voted != step.voted || result.Done || result.Threshold != voteThreshold {
					t.Fatalf("%s toggle: got %+v, want count=%d voted=%t", step.voter, result, step.count, step.voted)
				}
			}
		})
	}
}

func TestVoteSnapshot(t *testing.T) {
	vs := newVoteStore(&roomService{})
	toggleVote(t, vs, "kick", "anon-test", "alice")
	toggleVote(t, vs, "kick", "anon-test", "bob")
	toggleVote(t, vs, "kick", "anon-test", "alice")
	for voter, wantVoted := range map[string]bool{"alice": false, "bob": true} {
		w := httptest.NewRecorder()
		vs.list(w, httptest.NewRequest(http.MethodGet, "/votes?voter="+voter, nil), "test")
		var state voteSnapshot
		if err := json.Unmarshal(w.Body.Bytes(), &state); err != nil {
			t.Fatal(err)
		}
		if state.Counts["kick:anon-test"] != 1 || state.Voted["kick:anon-test"] != wantVoted {
			t.Fatalf("%s snapshot: %+v", voter, state)
		}
	}
	w := httptest.NewRecorder()
	vs.list(w, httptest.NewRequest(http.MethodGet, "/votes", nil), "test")
	var counts map[string]int
	if err := json.Unmarshal(w.Body.Bytes(), &counts); err != nil || counts["kick:anon-test"] != 1 {
		t.Fatalf("legacy counts: %s, %v", w.Body.String(), err)
	}
	vs.v[voteKey{"test", "kick", "anon-test"}]["bob"] = time.Now().Add(-voteTTL - time.Second)
	result := toggleVote(t, vs, "kick", "anon-test", "bob")
	if result.Count != 1 || !result.Voted {
		t.Fatalf("expired vote must be added again, got %+v", result)
	}
}
