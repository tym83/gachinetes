package replica

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tym83/gachinetes/pkg/tape"
)

// The whole way back: shipped to the bucket, the cluster lost, a tape file
// from the bucket at a revision, a store made from it by kuberoot-store and
// started, its tape read at every revision. KUBEROOT_STORE names the
// kuberoot-store binary; without it the test is skipped, and a binary that
// cannot restore a tape fails it.
func TestKuberootStoreRestoresTheBucket(t *testing.T) {
	bin := os.Getenv("KUBEROOT_STORE")
	if bin == "" {
		t.Skip("KUBEROOT_STORE is not set")
	}
	if out, _ := exec.Command(bin, "tape", "restore").CombinedOutput(); !strings.Contains(string(out), "usage: kuberoot-store tape restore") {
		t.Fatalf("%s cannot restore a tape: %s", bin, out)
	}
	r := newRig(t)
	r.tape.write(put("/registry/configmaps/shop/a", "1"), put("/registry/configmaps/shop/b", "1"))
	r.tape.write(put("/registry/configmaps/shop/a", "2"))
	r.sync(t)
	r.tape.write(del("/registry/configmaps/shop/b"))
	mid := r.tape.write(put("/registry/configmaps/shop/c", "1"))
	r.tape.write(put("/registry/configmaps/shop/a", "3"))
	r.sync(t)

	_, raw := r.restore(t, Target{Revision: mid})
	dir := t.TempDir()
	file := filepath.Join(dir, "tape.jsonl")
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "store")
	if out, err := exec.Command(bin, "tape", "restore", file, "--data-dir", data, "--revision", fmt.Sprint(mid)).CombinedOutput(); err != nil {
		t.Fatalf("kuberoot-store tape restore: %v: %s", err, out)
	}
	ports := freePorts(t, 3)
	tapeURL := "http://127.0.0.1:" + ports[2]
	cmd := exec.Command(bin, "--data-dir", data, "--listen-client-urls", "http://127.0.0.1:"+ports[0],
		"--listen-peer-urls", "http://127.0.0.1:"+ports[1], "--keep-history", "--listen-tape-urls", tapeURL)
	log, err := os.Create(filepath.Join(dir, "store.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _, _ = cmd.Process.Wait() })
	st := &tape.Store{URL: tapeURL}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var info tape.Info
	for {
		if info, err = st.Info(ctx); err == nil {
			break
		}
		if ctx.Err() != nil {
			out, _ := os.ReadFile(filepath.Join(dir, "store.log"))
			t.Fatalf("the restored store did not come up: %v\n%s", err, out)
		}
		time.Sleep(200 * time.Millisecond)
	}
	if info.Current != mid || info.From != 1 {
		t.Fatalf("restored tape: %+v, want 1 to %d", info, mid)
	}
	t.Logf("restored tape spans revisions %d to %d", info.From, info.Current)
	for rev := int64(1); rev <= mid; rev++ {
		got, err := st.RawRange(ctx, rev)
		if err != nil {
			t.Fatal(err)
		}
		if fmt.Sprint(replay(got.KVs, rev)) != fmt.Sprint(r.tape.stateAt(rev)) {
			t.Fatalf("revision %d:\nwant %v\ngot  %v", rev, r.tape.stateAt(rev), replay(got.KVs, rev))
		}
		// Revision 1 had no time; its base is stamped when it was read.
		if rev > 1 && got.Time != r.tape.timeOf(rev) {
			t.Fatalf("revision %d at %d, was %d", rev, got.Time, r.tape.timeOf(rev))
		}
		t.Logf("revision %d restored: %d keys match the tape", rev, len(replay(got.KVs, rev)))
	}
	graves, err := st.Graveyard(ctx, "/registry/")
	if err != nil {
		t.Fatal(err)
	}
	if len(graves) != 1 || graves[0].Key != "/registry/configmaps/shop/b" {
		t.Fatalf("graveyard: %+v", graves)
	}
	t.Logf("graveyard restored: %s", graves[0].Key)
}

func freePorts(t *testing.T, n int) []string {
	t.Helper()
	var out []string
	var ls []net.Listener
	for i := 0; i < n; i++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ls = append(ls, l)
		_, p, _ := net.SplitHostPort(l.Addr().String())
		out = append(out, p)
	}
	for _, l := range ls {
		l.Close()
	}
	return out
}
