package bootcfg

import "testing"

// Het configbestand-formaat: één sleutel per regel, commentaar weg, waarden mét
// spaties intact.
func TestAll(t *testing.T) {
	const text = "# HopOS node config\r\n" + `
hopos.node=hopos-lrv
hopos.insecure=1
hopos.init[]={"name":"welcome","ports":{"http":80}}
hopos.init[]={"name":"tweede", "cmd":"met spaties"}
   hopos.console=5555
`
	if got := All(text, "hopos.node"); len(got) != 1 || got[0] != "hopos-lrv" {
		t.Errorf(`All("hopos.node") = %q, wil ["hopos-lrv"]`, got)
	}
	if got := All(text, "hopos.init[]"); len(got) != 2 {
		t.Fatalf(`All("hopos.init[]") = %q, wil twee jobs`, got)
	}
	// Een waarde is de REST VAN DE REGEL, dus spaties in een jobspec zijn geen
	// probleem meer (dat was de enige reden dat de templates compacte JSON
	// eisten).
	if got := All(text, "hopos.init[]")[1]; got != `{"name":"tweede", "cmd":"met spaties"}` {
		t.Errorf("tweede jobspec = %q — waarde met spaties werd afgekapt", got)
	}
	// Ingesprongen regels horen gewoon te werken; een bestand is geen assembly.
	if got := All(text, "hopos.console"); len(got) != 1 || got[0] != "5555" {
		t.Errorf(`All("hopos.console") = %q, wil ["5555"]`, got)
	}
	if got := All(text, "hopos.cluster"); got != nil {
		t.Errorf(`All("hopos.cluster") = %q, wil nil`, got)
	}
}

// DE REGRESSIE: een uitgecommentarieerde regel is commentaar, óók met een spatie
// achter de #. De oude Fields-lezing maakte hier twee tokens van ("#" en
// "hopos.insecure=1") en zette de auth-poort dus open op een config die hem
// expliciet uitzette.
func TestCommentaarMetSpatieIsGeenConfig(t *testing.T) {
	for _, text := range []string{
		"# hopos.insecure=1\n",
		"#hopos.insecure=1\n",
		"#\thopos.insecure=1\n",
		"   # hopos.insecure=1\n",
	} {
		if got := All(text, "hopos.insecure"); got != nil {
			t.Errorf("All(%q) = %q — dit is commentaar, geen config", text, got)
		}
	}
	// En de niet-uitgecommentarieerde vorm moet natuurlijk wél werken.
	if got := All("hopos.insecure=1\n", "hopos.insecure"); len(got) != 1 || got[0] != "1" {
		t.Errorf(`All("hopos.insecure=1") = %q, wil ["1"]`, got)
	}
}

// De cmdline is het andere formaat: tokens op één regel, waarden zonder
// spaties, Linux-restanten negeren.
func TestCmdline(t *testing.T) {
	const args = "console=serial0,115200 root=/dev/mmcblk0p2 hopos.node=hop-1 hopos.init[]={\"name\":\"a\"} hopos.init[]={\"name\":\"b\"}"
	if got := Cmdline(args, "hopos.node"); len(got) != 1 || got[0] != "hop-1" {
		t.Errorf(`Cmdline("hopos.node") = %q, wil ["hop-1"]`, got)
	}
	if got := Cmdline(args, "hopos.init[]"); len(got) != 2 {
		t.Errorf(`Cmdline("hopos.init[]") = %q, wil twee`, got)
	}
	if got := Cmdline(args, "hopos.cores"); got != nil {
		t.Errorf(`Cmdline("hopos.cores") = %q, wil nil`, got)
	}
}

func TestFirst(t *testing.T) {
	if got := First(nil); got != "" {
		t.Errorf("First(nil) = %q, wil leeg", got)
	}
	if got := First([]string{"a", "b"}); got != "a" {
		t.Errorf("First = %q, wil \"a\"", got)
	}
}
