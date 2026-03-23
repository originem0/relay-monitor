//go:build windows

package notifier

import (
	"log"
	"sync"
	"time"

	"github.com/go-toast/toast"
)

// Notifier sends system notifications for critical relay events.
type Notifier struct {
	mu       sync.Mutex
	lastSent map[string]time.Time // provider -> last notification time
	cooldown time.Duration
}

func New() *Notifier {
	return &Notifier{
		lastSent: make(map[string]time.Time),
		cooldown: 5 * time.Minute,
	}
}

// Notify sends a Windows Toast notification if cooldown allows.
func (n *Notifier) Notify(title, body, providerKey string) {
	n.mu.Lock()
	if last, ok := n.lastSent[providerKey]; ok && time.Since(last) < n.cooldown {
		n.mu.Unlock()
		return
	}
	n.lastSent[providerKey] = time.Now()
	n.mu.Unlock()

	notification := toast.Notification{
		AppID:   "Relay Monitor",
		Title:   title,
		Message: body,
	}
	if err := notification.Push(); err != nil {
		log.Printf("[notifier] toast error: %v", err)
	}
}

// HandleEvent checks if an event type warrants a notification and sends it.
func (n *Notifier) HandleEvent(eventType, provider, model, oldValue, newValue, message string) {
	switch eventType {
	case "status_changed":
		if oldValue == "correct" && newValue == "wrong" {
			n.Notify("模型异常", provider+"/"+model+": 答案错误", provider)
		}
		if oldValue == "ok" && newValue == "error" {
			n.Notify("模型故障", provider+"/"+model+": "+message, provider)
		}
	case "provider_state_changed":
		if newValue == "down" || newValue == "error" {
			n.Notify("站点宕机", provider+": "+message, provider)
		}
	case "balance_low":
		n.Notify("余额不足", provider+": "+message, provider+"_balance")
	}
}
