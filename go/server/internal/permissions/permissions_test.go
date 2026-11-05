package permissions

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"github.com/pashagolub/pgxmock/v6"

	"github.com/jeffdafoe/llm-memory-api/go/server/internal/apierr"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/httpapi"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/jsonutil"
	"github.com/jeffdafoe/llm-memory-api/go/server/internal/testutil"
)

var nsCols = []string{"namespace", "can_read", "can_write", "can_delete"}

func TestNamespaceAccess(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("FROM namespace_permissions").WithArgs(4).
		WillReturnRows(pgxmock.NewRows(nsCols).
			AddRow("shared", true, true, false).
			AddRow("home", true, false, false))

	n := &Namespaces{DB: mock}
	ctx := context.Background()
	cases := []struct {
		namespace string
		op        Operation
		want      bool
	}{
		{"work", Delete, true}, // own namespace, no row needed
		{"shared", Read, true},
		{"shared", Write, true},
		{"shared", Delete, false},
		{"home", Write, false},
		{"other", Read, false},
	}
	for _, tc := range cases {
		got, err := n.HasAccess(ctx, 4, "work", tc.namespace, tc.op)
		if err != nil || got != tc.want {
			t.Errorf("%s %s = %v, %v; want %v", tc.namespace, tc.op, got, err, tc.want)
		}
	}
	// One query for all checks: the grants are cached.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	err := n.RequireAccess(ctx, 4, "work", "other", Write)
	status, code := apierr.StatusAndCode(err)
	if status != 403 || code != "FORBIDDEN" || err.Error() != `Actor "work" does not have write access to namespace "other"` {
		t.Fatalf("RequireAccess = %d %s %v", status, code, err)
	}

	names, all, err := n.ReadableNamespaces(ctx, 4, "work")
	if err != nil || all || !reflect.DeepEqual(names, []string{"shared", "home", "work"}) {
		t.Fatalf("ReadableNamespaces = %v, %v, %v", names, all, err)
	}
}

func TestNamespaceWildcard(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("FROM namespace_permissions").WithArgs(1).
		WillReturnRows(pgxmock.NewRows(nsCols).AddRow("/", true, true, true))

	n := &Namespaces{DB: mock}
	ctx := context.Background()
	ok, err := n.HasAccess(ctx, 1, "jeff", "anything", Delete)
	if err != nil || !ok {
		t.Fatalf("wildcard delete = %v, %v", ok, err)
	}
	names, all, err := n.ReadableNamespaces(ctx, 1, "jeff")
	if err != nil || !all || names != nil {
		t.Fatalf("ReadableNamespaces = %v, %v, %v", names, all, err)
	}
}

func TestNamespaceClearCache(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("FROM namespace_permissions").WithArgs(4).WillReturnRows(pgxmock.NewRows(nsCols))
	mock.ExpectQuery("FROM namespace_permissions").WithArgs(4).
		WillReturnRows(pgxmock.NewRows(nsCols).AddRow("shared", true, false, false))

	n := &Namespaces{DB: mock}
	ctx := context.Background()
	ok, _ := n.HasAccess(ctx, 4, "work", "shared", Read)
	if ok {
		t.Fatal("no grant yet")
	}
	n.ClearCache(4)
	ok, _ = n.HasAccess(ctx, 4, "work", "shared", Read)
	if !ok {
		t.Fatal("grant not seen after ClearCache")
	}
}

func TestValidateNamespace(t *testing.T) {
	for _, ns := range []string{"/", "*"} {
		err := ValidateNamespace(ns)
		status, _ := apierr.StatusAndCode(err)
		if status != 400 {
			t.Fatalf("%s: %v", ns, err)
		}
	}
	if ValidateNamespace("shared") != nil {
		t.Fatal("shared refused")
	}
}

var adminCols = []string{"resource", "action"}

func TestAdminHierarchy(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("FROM admin_permissions").WithArgs(5).
		WillReturnRows(pgxmock.NewRows(adminCols).
			AddRow("notes", "write").
			AddRow("agents", "create_system_equivalent").
			AddRow("comms", "*"))

	a := &Admin{DB: mock}
	ctx := context.Background()
	cases := []struct {
		resource, action string
		want             bool
	}{
		{"notes", "read", true},
		{"notes", "write", true},
		{"notes", "delete", false},
		{"agents", "create_system_equivalent", true},
		{"agents", "read", false},
		{"comms", "delete", true},
		{"dashboard", "read", false},
		{"*", "*", false},
	}
	for _, tc := range cases {
		got, err := a.HasPermission(ctx, 5, tc.resource, tc.action)
		if err != nil || got != tc.want {
			t.Errorf("%s:%s = %v, %v; want %v", tc.resource, tc.action, got, err, tc.want)
		}
	}
}

func TestAdminSuperadminAndMap(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("FROM admin_permissions").WithArgs(1).
		WillReturnRows(pgxmock.NewRows(adminCols).
			AddRow("notes", "read").
			AddRow("*", "*").
			AddRow("notes", "write").
			AddRow("agents", "read"))

	a := &Admin{DB: mock}
	ctx := context.Background()
	ok, err := a.IsSuperadmin(ctx, 1)
	if err != nil || !ok {
		t.Fatalf("IsSuperadmin = %v, %v", ok, err)
	}
	m, err := a.PermissionMap(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := jsonutil.Marshal(m)
	if string(got) != `{"notes":["read","write"],"*":["*"],"agents":["read"]}` {
		t.Fatalf("PermissionMap JSON = %s", got)
	}
}

func TestRequirePerm(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("FROM admin_permissions").WithArgs(5).
		WillReturnRows(pgxmock.NewRows(adminCols).AddRow("notes", "read"))
	mock.ExpectQuery("FROM admin_permissions").WithArgs(6).WillReturnError(errors.New("db down"))

	a := &Admin{DB: mock}
	reached := false
	h := a.RequirePerm("notes", "write", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { reached = true }))

	serve := func(actorID int) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		r := httpapi.WithStateValue(httptest.NewRequest("POST", "/v1/admin/notes/save", nil), &httpapi.State{ActorID: actorID})
		h.ServeHTTP(rec, r)
		return rec
	}

	forbidden := `{"error":{"code":"FORBIDDEN","message":"Insufficient permissions"}}`
	if rec := serve(0); rec.Code != 403 || rec.Body.String() != forbidden {
		t.Fatalf("no actor: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(5); rec.Code != 403 || rec.Body.String() != forbidden {
		t.Fatalf("read only: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(6); rec.Code != 500 || rec.Body.String() != `{"error":{"code":"INTERNAL","message":"Permission check failed"}}` {
		t.Fatalf("db error: %d %s", rec.Code, rec.Body.String())
	}
	if reached {
		t.Fatal("handler reached without permission")
	}
}

func TestIsTrustedCreator(t *testing.T) {
	mock := testutil.NewMock(t)
	mock.ExpectQuery("WHERE name = 'system'").WillReturnRows(pgxmock.NewRows([]string{"id"}).AddRow(2))
	mock.ExpectQuery("FROM admin_permissions").WithArgs(5).
		WillReturnRows(pgxmock.NewRows(adminCols).AddRow("agents", "create_system_equivalent"))
	mock.ExpectQuery("FROM admin_permissions").WithArgs(6).WillReturnRows(pgxmock.NewRows(adminCols))

	a := &Admin{DB: mock}
	ctx := context.Background()
	for actorID, want := range map[int]bool{2: true, 5: true, 6: false} {
		got, err := a.IsTrustedCreator(ctx, actorID)
		if err != nil || got != want {
			t.Errorf("actor %d = %v, %v; want %v", actorID, got, err, want)
		}
	}
}
