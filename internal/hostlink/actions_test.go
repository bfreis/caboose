package hostlink

import (
	"reflect"
	"testing"
)

// The Linux dialogs read their text as markup: zenity is told not to,
// and kdialog's is escaped, so what the sandbox put in a URL shows as
// written and cannot style or hide the rest.
func TestDialogToolsEscapeMarkup(t *testing.T) {
	got := dialogTools("open this?\n\nhttps://x.example/?a=<b>&c=</span>\\074")
	want := [][]string{
		{"zenity", "--question", "--title=caboose", "--no-markup", "--text", "open this?\n\nhttps://x.example/?a=<b>&c=</span>\\074"},
		{"kdialog", "--title", "caboose", "--yesno", "<qt>open this?<br><br>https://x.example/?a=&lt;b&gt;&amp;c=&lt;/span&gt;\\074</qt>"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %q\nwant %q", got, want)
	}
}
