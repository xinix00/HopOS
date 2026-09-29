package cfgblob

import "testing"

func TestAll(t *testing.T) {
	Text = `# HopOS node config
hopos.node=hopos-lrv
hopos.insecure=1
#hopos.apikey=nietdeze
hopos.init[]={"name":"welcome","ports":{"http":80}}
hopos.init[]={"name":"tweede"}
# hopos.cluster=ooknietdeze
`
	t.Cleanup(func() { Text = text })

	if got := All("hopos.node"); len(got) != 1 || got[0] != "hopos-lrv" {
		t.Errorf(`All("hopos.node") = %q, wil ["hopos-lrv"]`, got)
	}
	if got := All("hopos.init[]"); len(got) != 2 {
		t.Errorf(`All("hopos.init[]") = %q, wil twee jobs`, got)
	}
	// Uitgecommentarieerde regels horen niet mee te doen — met of zonder ruimte
	// achter de #. De lezing zelf is getest in fw/bootcfg; hier bewijzen we
	// alleen dat dit board diezelfde parser gebruikt.
	if got := All("hopos.apikey"); len(got) != 0 {
		t.Errorf(`All("hopos.apikey") = %q, wil niets — dat is een commentaarregel`, got)
	}
	if got := All("hopos.cluster"); len(got) != 0 {
		t.Errorf(`All("hopos.cluster") = %q, wil niets`, got)
	}
}

func TestAllZonderIngebakkenConfig(t *testing.T) {
	Text = ""
	t.Cleanup(func() { Text = text })
	if got := All("hopos.node"); got != nil {
		t.Errorf("All op een lege config = %q, wil nil", got)
	}
}
