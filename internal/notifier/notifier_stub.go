//go:build !windows

package notifier

// Notifier is a no-op on non-Windows platforms.
type Notifier struct{}

func New() *Notifier { return &Notifier{} }

func (n *Notifier) Notify(title, body, providerKey string) {}

func (n *Notifier) HandleEvent(eventType, provider, model, oldValue, newValue, message string) {}
