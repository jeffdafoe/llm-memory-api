package cleanup

import (
	"context"
	"errors"
	"regexp"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// The access request purge is one transaction: when a later statement fails,
// the invite delete before it is rolled back too, so an invite is never gone
// while the request it was for is still there half-purged.
func TestPurgeAccessRequestsFailureRollsBack(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	j := &Job{DB: mock, Config: storeOf(retentionKeys("30", "90", "0")), Log: &recorder{}}

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(expiredInvitesSQL)).WithArgs(pgxmock.AnyArg()).
		WillReturnRows(pgxmock.NewRows([]string{"access_request_id"}).AddRow(int64(7)).AddRow(int64(8)))
	mock.ExpectExec(regexp.QuoteMeta(invitedRequestsSQL)).WithArgs([]int64{7, 8}).WillReturnError(errors.New("connection reset"))
	mock.ExpectRollback()

	if _, _, err := j.PurgeAccessRequests(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Each error-log batch is its own statement, so when a later batch fails the
// earlier ones stay deleted. The partial event says what was deleted and
// which table failed, since Pass logs only the error.
func TestPurgeErrorLogsReportsCommittedRowsOnFailure(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.MatchExpectationsInOrder(true)
	rec := &recorder{}
	j := &Job{DB: mock, Config: storeOf(retentionKeys("0", "0", "90")), Log: rec}

	// error_log: one full batch, then a short one (done).
	mock.ExpectExec("DELETE FROM error_log").WithArgs(pgxmock.AnyArg(), errorLogPurgeBatch).
		WillReturnResult(pgxmock.NewResult("DELETE", errorLogPurgeBatch))
	mock.ExpectExec("DELETE FROM error_log").WithArgs(pgxmock.AnyArg(), errorLogPurgeBatch).
		WillReturnResult(pgxmock.NewResult("DELETE", 12))
	// system_errors: one full batch, then a failure.
	mock.ExpectExec("DELETE FROM system_errors").WithArgs(pgxmock.AnyArg(), errorLogPurgeBatch).
		WillReturnResult(pgxmock.NewResult("DELETE", errorLogPurgeBatch))
	mock.ExpectExec("DELETE FROM system_errors").WithArgs(pgxmock.AnyArg(), errorLogPurgeBatch).
		WillReturnError(errors.New("connection reset"))

	if _, _, err := j.PurgeErrorLogs(context.Background()); err == nil {
		t.Fatal("no error")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
	want := [3]string{"cleanup", "error-logs-partial", `{"error_log":5012,"system_errors":5000,"failedTable":"system_errors"}`}
	if len(rec.logs) != 1 || rec.logs[0] != want {
		t.Fatalf("logs %v\nwant %v", rec.logs, want)
	}
}

// The new keys drive hard deletes, so the whole value must be a whole number.
// Node's parseInt would read "1e3" as 1 and "7days" as 7, a shorter window
// than meant; here those are errors and the rule does not run.
func TestRetentionDaysParsing(t *testing.T) {
	cases := []struct {
		raw     string
		days    float64
		on, bad bool
	}{
		{"30", 30, true, false},
		{" 90 ", 90, true, false},
		{"0", 0, false, false},
		{"-5", 0, false, false},
		{"", 0, false, false},
		{"   ", 0, false, false},
		{"1e3", 0, false, true},
		{"7days", 0, false, true},
		{"3.5", 0, false, true},
		{"0x10", 0, false, true},
		{"+30", 0, false, true},
		{"1234567", 0, false, true},
	}
	for _, c := range cases {
		j := &Job{Config: storeOf(map[string]string{"k": c.raw}), Log: &recorder{}}
		days, on, err := j.retentionDays("k")
		if (err != nil) != c.bad || on != c.on || days != c.days {
			t.Errorf("%q: days %v on %v err %v", c.raw, days, on, err)
		}
	}
}

// A missing key is an error, so Pass logs it as cron-...-error, as it does
// for the other retention keys. Each purge reads its own keys.
func TestRetentionPurgesNeedTheirKeys(t *testing.T) {
	j := &Job{Config: storeOf(map[string]string{"access_request_retention_days": "30"}), Log: &recorder{}}
	if _, _, err := j.PurgeAccessRequests(context.Background()); err == nil {
		t.Error("access requests: no error without pending_access_request_retention_days")
	}
	if _, _, err := j.PurgeErrorLogs(context.Background()); err == nil {
		t.Error("error logs: no error without error_log_retention_days")
	}
}
