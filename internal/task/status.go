package task

import "slices"

// Task statuses. DESIGN.md names: taslak, hazır, alındı, çalışıyor, soru_bekliyor,
// review, bitti, teslim edildi, engellendi, iptal.
const (
	Draft     = "draft"
	Ready     = "ready"
	Claimed   = "claimed"
	Running   = "running"
	Waiting   = "waiting"
	Review    = "review"
	Done      = "done"      // finished, ready to deliver
	Delivered = "delivered" // handed over (`otag task deliver`); only from done
	Blocked   = "blocked"
	Cancelled = "cancelled"
)

// Statuses lists every status in lifecycle order.
var Statuses = []string{Draft, Ready, Claimed, Running, Waiting, Review, Done, Delivered, Blocked, Cancelled}

// transitions holds the forward edges; Blocked and Cancelled are reachable from every
// non-terminal status and are added by CanTransition.
var transitions = map[string][]string{
	Draft:   {Ready},
	Ready:   {Claimed, Draft},
	Claimed: {Running, Ready},
	Running: {Waiting, Review, Done, Ready}, // Ready: run crashed, retry with a new run
	// Running: the run received the answer (or a dialog closed). Claimed: the asking run
	// ended (parked) and the task's next run takes it over (ClaimWaiting). Review/Done:
	// `otag done` while a question is open. Ready: the run ended otherwise.
	Waiting: {Running, Claimed, Review, Done, Ready},
	Review:  {Done, Ready}, // Ready: review rejected, needs a new run
	Blocked: {Ready, Draft},
}

// Terminal reports whether s is a final status: no work is left. A done task still
// waits for its delivery (done -> delivered), which is not work.
func Terminal(s string) bool { return s == Done || s == Delivered || s == Cancelled }

// CanTransition reports whether from -> to is allowed.
func CanTransition(from, to string) bool {
	if from == Done && to == Delivered {
		return true
	}
	if Terminal(from) || from == to {
		return false
	}
	if to == Cancelled || (to == Blocked && from != Blocked) {
		return true
	}
	return slices.Contains(transitions[from], to)
}

// ValidStatus reports whether s is a known status.
func ValidStatus(s string) bool { return slices.Contains(Statuses, s) }
