package broker

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"persea-terminal/internal/terminal"
)

func TestShadowWitnessRemovedAfterMarkers(t *testing.T) {
	d := newDisposable(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := details(d.tmux, "$0")
	if err != nil {
		t.Fatal(err)
	}
	authority.SessionID, authority.SessionCreated = detail.ID, detail.Created
	witness, err := buildSourceWitness(ctx, d.tmux, authority.BootID, detail.ID)
	if err != nil {
		t.Fatal(err)
	}
	dir := shortTempDir(t)
	ready, release := shadowTestPipe(t, dir, "marked"), shadowTestPipe(t, dir, "release")
	installShadowTmuxWrapper(t, fmt.Sprintf(`if len(args)>8 and args[4]=='if-shell' and args[8].startswith('new-session '):
    boundary = args[8].find(' ; ', args[8].index(' @persea_broker_start '))
    before = args[8] if boundary < 0 else args[8][:boundary]
    after = 'display-message -p done' if boundary < 0 else args[8][boundary+3:]
    words = shlex.split(before.split(' ; ', 1)[0])
    name = words[words.index('-s')+1]
    subprocess.run([tmux,*args[:4],'if-shell','true',before], check=True)
    with open(%q,'w') as f:
        f.write(name+'\n')
    with open(%q) as f:
        f.readline()
    args[8] = after
`, ready.Name(), release.Name()))
	delayed := newDelayedPTY()
	delayed.setConsumer(func([]byte) error { return nil }, func(error) {})
	txn := &tmuxPinnedTransaction{server: d.tmux, bootID: authority.BootID, authority: authority, pty: delayed}
	type result struct {
		bound terminal.TransactionResult
		err   error
	}
	var got result
	done := make(chan struct{})
	go func() {
		got.bound, got.err = txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
		close(done)
	}()
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { _, _ = fmt.Fprintln(release, "release") }) }
	defer func() {
		unblock()
		<-done
		_, _ = txn.RunPinned(context.Background(), terminal.TransactionRequest{Action: terminal.ActionCleanup, Witness: got.bound.Witness, Attachment: got.bound.Attachment})
	}()
	name := shadowPipeLine(t, ready)
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	if flags := d.run("display-message", "-p", "-t", name, shadowMarkerFlags); flags != "1\t1\t1" {
		t.Fatalf("witness unset was scheduled before all markers: %q", flags)
	}
	owner, ownerErr := tmuxOutput(d.tmux, "show-environment", "-t", "="+id+":", attachmentOwnerEnvironment)
	process, err := (procProbe{}).Witness(ctx, os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	wantOwner := fmt.Sprintf("%s=%s:%d:%d\n", attachmentOwnerEnvironment, authority.BootID, process.PID, process.StartTime)
	if ownerErr != nil || owner != wantOwner {
		t.Fatalf("witness disappeared before final marker: %q %v", owner, ownerErr)
	}
	if got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10); got.Status != "ok" {
		t.Fatal(got)
	}
	unblock()
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("birth did not finish")
	}
	if got.err != nil {
		t.Fatal(got.err)
	}
	if out, err := tmuxOutput(d.tmux, "show-environment", "-t", "="+id+":", attachmentOwnerEnvironment); err == nil || !strings.Contains(err.Error(), "unknown variable: "+attachmentOwnerEnvironment) {
		t.Errorf("completed shadow retained witness: %q %v", out, err)
	}
	inherited := shadowTestPipe(t, dir, "inherited")
	d.run("new-window", "-d", "-t", "="+id+":", "printf '%s\\n' \"${PERSEA_SHADOW_OWNER-unset}\" > "+shellQuote(inherited.Name())+"; sleep 600")
	if value := shadowPipeLine(t, inherited); value != "unset" {
		t.Fatalf("later pane inherited birth witness: %q", value)
	}
}

func TestShadowMarkerRecheckAfterWitnessRemoval(t *testing.T) {
	d := newDisposable(t)
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	name := attachmentShadowPrefix + strings.Repeat("d", 32)
	d.run("new-session", "-d", "-t", "alpha", "-s", name, "-e", fmt.Sprintf("%s=%s:%d:%d", attachmentOwnerEnvironment, inc.BootID, owner.PID, owner.StartTime))
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	installShadowTmuxWrapper(t, fmt.Sprintf(`if args[4]=='show-environment':
    for option, value in [('@persea_client_id',%q),('@persea_broker_pid',%q),('@persea_broker_start',%q)]:
        subprocess.run([tmux,*args[:4],'set-option','-t',%q,option,value],check=True)
    subprocess.run([tmux,*args[:4],'set-environment','-u','-t',%q,'PERSEA_SHADOW_OWNER'],check=True)
`, attachmentClientPrefix+strings.Repeat("d", 32), fmt.Sprint(owner.PID), fmt.Sprint(owner.StartTime), name, name))
	logs := captureBrokerLogs(t)
	got := (&Server{config: bootCreationConfig(t, d.tmux)}).inventoryServer(d.tmux, 10)
	if got.Status != "ok" || got.Error != "" {
		t.Fatalf("birth completion blocked inventory: %+v", got)
	}
	if present, err := tmuxSessionPresent(d.tmux, id); err != nil || !present {
		t.Fatalf("marker recheck lost completed shadow: present=%t err=%v", present, err)
	}
	if !strings.Contains(logs.String(), "event=orphan_shadow_skipped") || !strings.Contains(logs.String(), "became protected") {
		t.Fatalf("guard refusal was not logged: %q", logs.String())
	}
}

func TestShadowWitnessUsesSessionEnvironment(t *testing.T) {
	d := newDisposable(t)
	inc, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	owner, err := (procProbe{}).Witness(context.Background(), os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	value := fmt.Sprintf("%s:%d:%d", inc.BootID, owner.PID, owner.StartTime)
	d.run("set-environment", "-g", attachmentOwnerEnvironment, value)
	name := attachmentShadowPrefix + strings.Repeat("e", 32)
	d.run("new-session", "-d", "-t", "alpha", "-s", name)
	id := d.run("display-message", "-p", "-t", name, "#{session_id}")
	if expanded := d.run("display-message", "-p", "-t", name, "#{"+attachmentOwnerEnvironment+"}"); expanded != value {
		t.Fatalf("format did not expand global environment: %q", expanded)
	}
	if alive, err := restoredShadowOwnerAlive(d.tmux, id, inc.BootID); err != nil || alive {
		t.Fatalf("global environment protected unrelated session: alive=%t err=%v", alive, err)
	}
}

func TestUnresolvedShadowBirthIsLogged(t *testing.T) {
	d := newDisposable(t)
	ctx := context.Background()
	authority, err := incarnation(d.tmux)
	if err != nil {
		t.Fatal(err)
	}
	detail, err := details(d.tmux, "$0")
	if err != nil {
		t.Fatal(err)
	}
	authority.SessionID, authority.SessionCreated = detail.ID, detail.Created
	witness, err := buildSourceWitness(ctx, d.tmux, authority.BootID, detail.ID)
	if err != nil {
		t.Fatal(err)
	}
	installShadowTmuxWrapper(t, `if args[4:6] == ['list-sessions', '-f']:
    sys.exit(0)
if len(args)>8 and args[4]=='if-shell' and args[8].startswith('new-session '):
    first = shlex.split(args[8].split(' ; ',1)[0])
    subprocess.run([tmux,*args[:4],*first], stdout=subprocess.DEVNULL, check=True)
    sys.exit(1)
`)
	logs := captureBrokerLogs(t)
	delayed := newDelayedPTY()
	delayed.setConsumer(func([]byte) error { return nil }, func(error) {})
	txn := &tmuxPinnedTransaction{server: d.tmux, bootID: authority.BootID, authority: authority, pty: delayed}
	bound, err := txn.RunPinned(ctx, terminal.TransactionRequest{Action: terminal.ActionBind, Witness: witness})
	if err == nil || bound.Attachment.ShadowSessionID != "" {
		t.Fatalf("missing ID fixture failed: %+v %v", bound, err)
	}
	names := strings.Fields(d.run("list-sessions", "-F", "#{session_name}"))
	if len(names) != 2 {
		t.Fatalf("lost birth fixture: %q", names)
	}
	for _, name := range names {
		if attachmentShadowName(name) && (strings.Count(logs.String(), "event=attachment_shadow_unresolved") != 1 || !strings.Contains(logs.String(), fmt.Sprintf("shadow=%q", name))) {
			t.Fatalf("unresolved shadow not identified once: %q", logs.String())
		}
	}
}
