package auth

import (
	"context"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

// Ported from node/api/src/services/password-login.test.js: the database is
// mocked, the hashing is real.

const loginSQL = `SELECT id, name, password_hash, password_salt FROM actors WHERE name = \$1 AND password_hash IS NOT NULL`

func expectAlice(t *testing.T, mock pgxmock.PgxPoolIface) {
	salt := GenerateSalt()
	hash, err := Hash("right password", salt)
	if err != nil {
		t.Fatal(err)
	}
	mock.ExpectQuery(loginSQL).WithArgs("alice").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "password_hash", "password_salt"}).AddRow(7, "alice", &hash, &salt))
}

func TestPasswordLoginRightPassword(t *testing.T) {
	mock := testutil.NewMock(t)
	expectAlice(t, mock)
	got, err := VerifyPasswordLogin(context.Background(), mock, "alice", "right password")
	if err != nil || got == nil || got.ID != 7 || got.Name != "alice" {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPasswordLoginWrongPassword(t *testing.T) {
	mock := testutil.NewMock(t)
	expectAlice(t, mock)
	got, err := VerifyPasswordLogin(context.Background(), mock, "alice", "wrong password")
	if err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
}

func TestPasswordLoginUnknownName(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery(loginSQL).WithArgs("nobody").
		WillReturnRows(pgxmock.NewRows([]string{"id", "name", "password_hash", "password_salt"}))
	got, err := VerifyPasswordLogin(context.Background(), mock, "nobody", "right password")
	if err != nil || got != nil {
		t.Fatalf("%v %v", got, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

func TestNodeHexPrefix(t *testing.T) {
	if got := nodeHexPrefix("0aFfzz12"); len(got) != 2 || got[0] != 0x0a || got[1] != 0xff {
		t.Fatalf("%x", got)
	}
	if got := nodeHexPrefix("abc"); len(got) != 1 {
		t.Fatalf("%x", got)
	}
}
