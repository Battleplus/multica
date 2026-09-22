package handler

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/multica-ai/multica/server/internal/events"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
	"github.com/multica-ai/multica/server/pkg/protocol"
)

type duplicateRelations struct {
	DuplicateOf *IssueResponse  `json:"duplicate_of"`
	Duplicates  []IssueResponse `json:"duplicates"`
}

func duplicateState(t *testing.T, issueID string) (status string, duplicateOf *string, revision int64) {
	t.Helper()
	dbfx.QueryRow(t, `SELECT status, duplicate_of_issue_id::text, revision FROM issue WHERE id = $1`, issueID).
		Scan(&status, &duplicateOf, &revision)
	return status, duplicateOf, revision
}

func updateIssueRequest(issueID string, body map[string]any) *http.Request {
	return withURLParam(newRequest("PUT", "/api/issues/"+issueID, body), "id", issueID)
}

func markDuplicate(t *testing.T, issueID, targetID string) *testutil.Response {
	t.Helper()
	return testutil.Call(t, testHandler.UpdateIssue, updateIssueRequest(issueID, map[string]any{
		"duplicate_of_issue_id": targetID,
	}))
}

func listDuplicates(t *testing.T, issueID string) duplicateRelations {
	t.Helper()
	var out duplicateRelations
	req := withURLParam(newRequest("GET", "/api/issues/"+issueID+"/duplicates", nil), "id", issueID)
	testutil.Call(t, testHandler.ListIssueDuplicates, req).Want(http.StatusOK).JSON(&out)
	return out
}

func TestMarkDuplicateCancelsAndLinksBothSides(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	original := dbfx.Issue(t, "dup-original", testutil.Cols{"status": "in_progress"})
	duplicate := dbfx.Issue(t, "dup-duplicate", testutil.Cols{"status": "todo"})

	// The mark is a status write: it must emit the same status_changed event a
	// manual cancel does, or parent stage barriers never hear about it.
	var mu sync.Mutex
	var payloads []map[string]any
	testHandler.Bus.Subscribe(protocol.EventIssueUpdated, func(e events.Event) {
		payload, ok := e.Payload.(map[string]any)
		if !ok {
			return
		}
		if resp, ok := payload["issue"].(IssueResponse); ok && resp.ID == duplicate {
			mu.Lock()
			payloads = append(payloads, payload)
			mu.Unlock()
		}
	})

	var resp IssueResponse
	markDuplicate(t, duplicate, original).Want(http.StatusOK).JSON(&resp)
	if resp.Status != "cancelled" {
		t.Fatalf("marked issue status = %q, want cancelled", resp.Status)
	}
	status, pointer, _ := duplicateState(t, duplicate)
	if status != "cancelled" || pointer == nil || *pointer != original {
		t.Fatalf("stored (status, duplicate_of) = (%q, %v), want (cancelled, %s)", status, pointer, original)
	}

	mu.Lock()
	if len(payloads) != 1 {
		mu.Unlock()
		t.Fatalf("got %d issue:updated events for the marked issue, want 1", len(payloads))
	}
	event := payloads[0]
	mu.Unlock()
	if event["status_changed"] != true {
		t.Errorf("status_changed = %v, want true", event["status_changed"])
	}
	if got, _ := event["duplicate_of_issue_id"].(*string); got == nil || *got != original {
		t.Errorf("event duplicate_of_issue_id = %v, want %s", event["duplicate_of_issue_id"], original)
	}

	fromDuplicate := listDuplicates(t, duplicate)
	if fromDuplicate.DuplicateOf == nil || fromDuplicate.DuplicateOf.ID != original {
		t.Fatalf("duplicate's relations: duplicate_of = %+v, want %s", fromDuplicate.DuplicateOf, original)
	}
	fromOriginal := listDuplicates(t, original)
	if fromOriginal.DuplicateOf != nil {
		t.Fatalf("original's relations: duplicate_of = %+v, want nil", fromOriginal.DuplicateOf)
	}
	if len(fromOriginal.Duplicates) != 1 || fromOriginal.Duplicates[0].ID != duplicate {
		t.Fatalf("original's relations: duplicates = %+v, want [%s]", fromOriginal.Duplicates, duplicate)
	}
}

func TestDuplicateMarkLivesOnlyWhileCancelled(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	original := dbfx.Issue(t, "dup-live-original")
	duplicate := dbfx.Issue(t, "dup-live-duplicate")
	markDuplicate(t, duplicate, original).Want(http.StatusOK)

	// Edits that do not touch status keep the mark.
	testutil.Call(t, testHandler.UpdateIssue, updateIssueRequest(duplicate, map[string]any{
		"title": "dup-live-duplicate renamed",
	})).Want(http.StatusOK)
	if _, pointer, _ := duplicateState(t, duplicate); pointer == nil {
		t.Fatal("a title edit cleared the duplicate mark")
	}

	// Moving off cancelled is how a mark is removed.
	testutil.Call(t, testHandler.UpdateIssue, updateIssueRequest(duplicate, map[string]any{
		"status": "todo",
	})).Want(http.StatusOK)
	status, pointer, _ := duplicateState(t, duplicate)
	if status != "todo" || pointer != nil {
		t.Fatalf("after reopening: (status, duplicate_of) = (%q, %v), want (todo, nil)", status, pointer)
	}
	if relations := listDuplicates(t, original); len(relations.Duplicates) != 0 {
		t.Fatalf("original still lists %d duplicates after the mark was removed", len(relations.Duplicates))
	}
}

func TestBackgroundStatusWriteClearsDuplicateMark(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	original := dbfx.Issue(t, "dup-bg-original")
	duplicate := dbfx.Issue(t, "dup-bg-duplicate")
	markDuplicate(t, duplicate, original).Want(http.StatusOK)

	// GitHub sync and task recovery write status through UpdateIssueStatus.
	if _, err := testHandler.Queries.UpdateIssueStatus(context.Background(), db.UpdateIssueStatusParams{
		ID:          parseUUID(duplicate),
		Status:      "done",
		WorkspaceID: parseUUID(testWorkspaceID),
	}); err != nil {
		t.Fatalf("UpdateIssueStatus: %v", err)
	}
	if status, pointer, _ := duplicateState(t, duplicate); status != "done" || pointer != nil {
		t.Fatalf("after background write: (status, duplicate_of) = (%q, %v), want (done, nil)", status, pointer)
	}
}

func TestRemarkingSameTargetIsNoop(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	original := dbfx.Issue(t, "dup-noop-original")
	duplicate := dbfx.Issue(t, "dup-noop-duplicate")
	markDuplicate(t, duplicate, original).Want(http.StatusOK)
	_, _, before := duplicateState(t, duplicate)

	markDuplicate(t, duplicate, original).Want(http.StatusOK)
	if _, _, after := duplicateState(t, duplicate); after != before {
		t.Fatalf("re-marking the same target bumped revision %d -> %d", before, after)
	}
}

func TestDuplicateMarkRejections(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	original := dbfx.Issue(t, "dup-reject-original")
	marked := dbfx.Issue(t, "dup-reject-marked")
	markDuplicate(t, marked, original).Want(http.StatusOK)
	other := dbfx.Issue(t, "dup-reject-other")

	t.Run("self", func(t *testing.T) {
		markDuplicate(t, other, other).Want(http.StatusBadRequest)
	})
	t.Run("target not in workspace", func(t *testing.T) {
		markDuplicate(t, other, "00000000-0000-0000-0000-000000000001").Want(http.StatusBadRequest)
	})
	t.Run("target is itself a duplicate", func(t *testing.T) {
		body := markDuplicate(t, other, marked).Want(http.StatusConflict).Map()
		if body["code"] != "duplicate_target_is_duplicate" {
			t.Fatalf("code = %v, want duplicate_target_is_duplicate", body["code"])
		}
	})
	t.Run("issue has duplicates", func(t *testing.T) {
		body := markDuplicate(t, original, other).Want(http.StatusConflict).Map()
		if body["code"] != "issue_has_duplicates" {
			t.Fatalf("code = %v, want issue_has_duplicates", body["code"])
		}
	})
	t.Run("explicit null", func(t *testing.T) {
		testutil.Call(t, testHandler.UpdateIssue, updateIssueRequest(marked, map[string]any{
			"duplicate_of_issue_id": nil,
		})).Want(http.StatusBadRequest)
	})
	t.Run("status other than cancelled", func(t *testing.T) {
		testutil.Call(t, testHandler.UpdateIssue, updateIssueRequest(other, map[string]any{
			"duplicate_of_issue_id": original,
			"status":                "done",
		})).Want(http.StatusBadRequest)
	})
	t.Run("batch update", func(t *testing.T) {
		req := newRequest("POST", "/api/issues/batch-update", map[string]any{
			"issue_ids": []string{other},
			"updates":   map[string]any{"duplicate_of_issue_id": original},
		})
		testutil.Call(t, testHandler.BatchUpdateIssues, req).Want(http.StatusBadRequest)
	})

	// None of the rejected writes may have touched the issue.
	if status, pointer, _ := duplicateState(t, other); status != "todo" || pointer != nil {
		t.Fatalf("rejected marks changed the issue: (status, duplicate_of) = (%q, %v)", status, pointer)
	}
}

func TestDeletingOriginalClearsDuplicateMarks(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	original := dbfx.Issue(t, "dup-delete-original")
	duplicate := dbfx.Issue(t, "dup-delete-duplicate")
	markDuplicate(t, duplicate, original).Want(http.StatusOK)

	req := withURLParam(newRequest("DELETE", "/api/issues/"+original, nil), "id", original)
	testutil.Call(t, testHandler.DeleteIssue, req).Want(http.StatusNoContent)

	status, pointer, _ := duplicateState(t, duplicate)
	if status != "cancelled" || pointer != nil {
		t.Fatalf("after deleting the original: (status, duplicate_of) = (%q, %v), want (cancelled, nil)", status, pointer)
	}
}

// Two people marking A as a duplicate of B and B as a duplicate of A at the
// same moment must not leave a cycle. Both marks lock the pair in id order, so
// whichever runs second sees the first one's pointer and is refused.
func TestConcurrentCrossMarksLeaveNoCycle(t *testing.T) {
	if testHandler == nil {
		t.Skip("database not available")
	}
	for attempt := 0; attempt < 5; attempt++ {
		a := dbfx.Issue(t, "dup-race-a")
		b := dbfx.Issue(t, "dup-race-b")

		var start, done sync.WaitGroup
		start.Add(1)
		codes := make([]int, 2)
		for i, pair := range [][2]string{{a, b}, {b, a}} {
			done.Add(1)
			go func(i int, issueID, targetID string) {
				defer done.Done()
				start.Wait()
				codes[i] = markDuplicate(t, issueID, targetID).Code
			}(i, pair[0], pair[1])
		}
		start.Done()
		done.Wait()

		succeeded := 0
		for _, code := range codes {
			switch code {
			case http.StatusOK:
				succeeded++
			case http.StatusConflict:
			default:
				t.Fatalf("attempt %d: unexpected status %d (codes %v)", attempt, code, codes)
			}
		}
		if succeeded != 1 {
			t.Fatalf("attempt %d: %d marks succeeded, want exactly 1 (codes %v)", attempt, succeeded, codes)
		}
		_, aPointer, _ := duplicateState(t, a)
		_, bPointer, _ := duplicateState(t, b)
		if aPointer != nil && bPointer != nil {
			t.Fatalf("attempt %d: cycle left behind: %s -> %s and %s -> %s", attempt, a, *aPointer, b, *bPointer)
		}
	}
}
