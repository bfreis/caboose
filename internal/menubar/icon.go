package menubar

import "fmt"

// IconInput is what the icon is decided from.
type IconInput struct {
	// Disconnected: no daemon for over DisconnectedAfter, or one that
	// refuses this app's protocol.
	Disconnected bool
	Running      int // sandboxes running
	Attention    int // things that need the user
}

// Icon decides the status item's look. The image says only whether a
// sandbox runs; the count beside it says only what needs the user, so
// each can be read without the other. The description says both, in
// counts alone: never a sandbox's name or text.
func Icon(in IconInput) IconState {
	if in.Disconnected {
		return IconState{Image: ImageIdle, Title: " !", Description: "caboose: the daemon is not answering"}
	}
	s := IconState{Image: ImageIdle, Description: "caboose: nothing running"}
	if in.Running > 0 {
		s.Image = ImageRunning
		s.Description = fmt.Sprintf("caboose: %d running", in.Running)
	}
	if in.Attention > 0 {
		s.Title = fmt.Sprintf(" %d", in.Attention)
		s.Description += "; " + plural(in.Attention, "thing needs", "things need") + " you"
	}
	return s
}
