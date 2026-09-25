package flintlock

import (
	"context"
	"encoding/base64"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/liquidmetal-dev/flintlock/api/types"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/ismoilovdevml/firerunner/internal/config"
	"github.com/ismoilovdevml/firerunner/internal/flintlock/flintlocktest"
)

var errTransient = status.Error(codes.Unknown, "getting microvm spec: failed reading from content store")

func testClient(t *testing.T, srv *flintlocktest.Server) *Client {
	t.Helper()
	return newClient(flintlocktest.StartBufconn(t, srv), "ns1")
}

func mvm(id, uid string) *types.MicroVM {
	return &types.MicroVM{Spec: &types.MicroVMSpec{Id: id, Uid: &uid}}
}

func TestCreate(t *testing.T) {
	cases := []struct {
		name    string
		uid     string
		wantErr bool
	}{
		{"returns uid and sets namespace", "uid-1", false},
		{"empty uid is an error", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := flintlocktest.NewServer(c.uid)
			fl := testClient(t, srv)
			got, err := fl.Create(context.Background(), &types.MicroVMSpec{Id: "job-1", Namespace: "other"})
			if (err != nil) != c.wantErr || got != c.uid {
				t.Fatalf("Create = %q, %v; want %q, err %v", got, err, c.uid, c.wantErr)
			}
			specs := srv.Created()
			if len(specs) != 1 || specs[0].GetNamespace() != "ns1" {
				t.Fatalf("server got %v, want one spec in namespace ns1", specs)
			}
		})
	}
}

func TestDeletePassesUID(t *testing.T) {
	srv := flintlocktest.NewServer("")
	if err := testClient(t, srv).Delete(context.Background(), "uid-9"); err != nil {
		t.Fatal(err)
	}
	if d := srv.Deleted(); len(d) != 1 || d[0] != "uid-9" {
		t.Fatalf("deleted %v", d)
	}
}

func TestListRetries(t *testing.T) {
	cases := []struct {
		name      string
		errs      []error
		wantErr   bool
		wantCalls int
	}{
		{"success", nil, false, 1},
		{"transient error is retried", []error{errTransient}, false, 2},
		{"other error is not retried", []error{status.Error(codes.Unavailable, "connection refused")}, true, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := flintlocktest.NewServer("")
			srv.SetVMs(mvm("pool-a", "u1"))
			srv.FailList(c.errs...)
			vms, err := testClient(t, srv).List(context.Background())
			if (err != nil) != c.wantErr {
				t.Fatalf("List err = %v, want err %v", err, c.wantErr)
			}
			if !c.wantErr && (len(vms) != 1 || vms[0].GetSpec().GetUid() != "u1") {
				t.Fatalf("List = %v", vms)
			}
			if n := srv.ListCalls(); n != c.wantCalls {
				t.Fatalf("ListMicroVMs called %d times, want %d", n, c.wantCalls)
			}
		})
	}
}

func TestListHonoursContext(t *testing.T) {
	srv := flintlocktest.NewServer("")
	srv.FailList(errTransient, errTransient, errTransient, errTransient, errTransient, errTransient)
	fl := testClient(t, srv)
	// The first backoff is 300ms; the deadline must end the wait long before it.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := fl.List(ctx)
	if !errors.Is(err, context.DeadlineExceeded) && status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("List err = %v, want deadline exceeded", err)
	}
	if el := time.Since(start); el > 250*time.Millisecond {
		t.Fatalf("List took %s after the deadline", el)
	}
	if n := srv.ListCalls(); n != 1 {
		t.Fatalf("ListMicroVMs called %d times after cancel, want 1", n)
	}
}

func TestFind(t *testing.T) {
	srv := flintlocktest.NewServer("")
	srv.SetVMs(mvm("pool-a", "u1"), mvm("job-7", "u2"))
	fl := testClient(t, srv)
	cases := []struct {
		ref, wantUID string
		wantErr      bool
	}{
		{"job-7", "u2", false},
		{"u1", "u1", false},
		{"nope", "", true},
	}
	for _, c := range cases {
		v, err := fl.Find(context.Background(), c.ref)
		if (err != nil) != c.wantErr || v.GetSpec().GetUid() != c.wantUID {
			t.Errorf("Find(%q) = %v, %v; want uid %q err %v", c.ref, v, err, c.wantUID, c.wantErr)
		}
	}
}

func TestFindPropagatesListError(t *testing.T) {
	srv := flintlocktest.NewServer("")
	srv.FailList(status.Error(codes.Unavailable, "down"))
	if _, err := testClient(t, srv).Find(context.Background(), "x"); status.Code(err) != codes.Unavailable {
		t.Fatalf("Find err = %v, want Unavailable", err)
	}
}

func TestBasicAuthMetadata(t *testing.T) {
	md, err := basicAuth("tok").GetRequestMetadata(context.Background())
	want := "basic " + base64.StdEncoding.EncodeToString([]byte("tok"))
	if err != nil || md["authorization"] != want {
		t.Fatalf("metadata = %v, %v; want %q", md, err, want)
	}
	if basicAuth("tok").RequireTransportSecurity() {
		t.Fatal("flintlockd is plaintext on localhost; transport security must not be required")
	}
}

func TestDialSendsToken(t *testing.T) {
	srv := flintlocktest.NewServer("")
	cfg := flintlocktest.StartUnix(t, srv)
	fl, err := Dial(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer fl.Close()
	if fl.namespace != "test" {
		t.Fatalf("namespace %q", fl.namespace)
	}
	if _, err := fl.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	// The token file ends in a newline; Dial must trim it.
	want := "basic " + base64.StdEncoding.EncodeToString([]byte(flintlocktest.Token))
	if a := srv.Auth(); len(a) != 1 || a[0] != want {
		t.Fatalf("server saw authorization %v, want %q", a, want)
	}
}

func TestDialMissingToken(t *testing.T) {
	_, err := Dial(config.Flintlock{Endpoint: "127.0.0.1:1", TokenFile: filepath.Join(t.TempDir(), "none")})
	if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), "token") {
		t.Fatalf("Dial err = %v, want wrapped ErrNotExist", err)
	}
}

// OnError sees every failed call with its RPC name, each failed try of a
// retried listing included, and nothing that succeeded.
func TestOnError(t *testing.T) {
	srv := flintlocktest.NewServer("uid-1")
	fl := testClient(t, srv)
	var got []string
	fl.OnError = func(op string, err error) { got = append(got, op+":"+status.Code(err).String()) }
	srv.FailList(errTransient, status.Error(codes.Unavailable, "down"))
	srv.FailDelete(status.Error(codes.NotFound, "gone"))
	_, _ = fl.List(context.Background()) // transient, then Unavailable (not retried)
	_, _ = fl.List(context.Background())
	_ = fl.Delete(context.Background(), "u1")
	_ = fl.Delete(context.Background(), "u2")
	_, _ = fl.Create(context.Background(), &types.MicroVMSpec{Id: "job-1"})
	want := []string{"ListMicroVMs:Unknown", "ListMicroVMs:Unavailable", "DeleteMicroVM:NotFound"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Fatalf("OnError saw %v, want %v", got, want)
	}
}
