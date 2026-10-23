//go:build !darwin

package macui

import (
	"io"

	"github.com/bfreis/caboose/internal/menubar"
)

// App is the menu-bar app's glue, which exists on macOS only: here Run
// and every call that could fail say so, and the rest do nothing.
type App struct{}

var _ menubar.UI = (*App)(nil)

// New returns the glue.
func New() *App { return &App{} }

// SelfTest has nothing to check on this system.
func SelfTest(io.Writer) error { return ErrMacOnly }

func (*App) Run(menubar.Events, func()) error       { return ErrMacOnly }
func (*App) SetIcon(menubar.IconState)              {}
func (*App) SetMenu(menubar.Menu)                   {}
func (*App) Confirm(menubar.Confirmation)           {}
func (*App) AskURL(menubar.URLDialog)               {}
func (*App) Dismiss(string)                         {}
func (*App) Notify(menubar.Notification)            {}
func (*App) Withdraw([]string)                      {}
func (*App) Open(string) error                      { return ErrMacOnly }
func (*App) LoginItem() (menubar.LoginState, error) { return menubar.LoginUnavailable, ErrMacOnly }
func (*App) SetLoginItem(bool) error                { return ErrMacOnly }
func (*App) OpenLoginSettings() error               { return ErrMacOnly }
func (*App) Alert(string, string)                   {}
func (*App) Quit()                                  {}
