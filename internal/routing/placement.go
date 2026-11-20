package routing

// Placement records the inputs that affected the selected account at session
// creation. An empty Weighted means no measured quota was recorded.
type Placement struct {
	Account     string
	Reason      string
	Eligibility string
	Window      string  `json:",omitempty"`
	Remaining   float64 `json:",omitempty"`
	Capacity    float64 `json:",omitempty"`
	Weighted    string  `json:",omitempty"`
	Skipped     int     `json:",omitempty"`
}

const (
	Pinned          = "Pinned account"
	OnlyAccount     = "Only connected account"
	HighestQuota    = "Highest weighted quota"
	RandomTie       = "Randomly selected among tied accounts"
	ParentTie       = "Parent account won the tie"
	NameTie         = "Account name broke the tie"
	UnknownQuota    = "No eligible measured quota"
	OpeningFallback = "Fallback to first connected account"
	SetupFallback   = "Fallback after earlier accounts were skipped or failed"
	ForkAccount     = "Inherited the connected account"
)

const (
	probeFailed  = "Probe failed"
	probeMissing = "Probe did not return"
)

func (q Quota) eligibility() string {
	switch {
	case q.coolingDown:
		return "On cooldown"
	case q.Err != nil:
		return probeFailed
	case q.Name == "":
		return probeMissing
	case q.Remaining == 0:
		return "Exhausted"
	case q.Remaining < 0:
		return "Eligible with unknown quota"
	default:
		return "Eligible"
	}
}

func (q Quota) placement(name, reason string) Placement {
	p := Placement{Account: name, Reason: reason, Eligibility: q.eligibility()}
	if q.Name != "" && q.Err == nil && q.Remaining >= 0 {
		p.Window, p.Remaining, p.Capacity = q.Window, q.Remaining, q.Capacity
		score := weightedQuota(q.Remaining, q.Capacity)
		score.Quo(score, weightedQuota(100, 1))
		p.Weighted = score.Text('g', 6)
	}
	return p
}
