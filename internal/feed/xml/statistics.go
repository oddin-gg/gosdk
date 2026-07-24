package xml

// StatisticsPair is a home/away statistic. Both sides are POINTERS so an
// ABSENT attribute stays absent: pre-fix they were plain ints, so
// `<yellow_cards home="2"/>` (no away attribute) decoded away as a
// present zero and surfaced publicly as Some(0) — indistinguishable from
// a real "away = 0". One-sided feed payloads are common, so this made
// partial data materially wrong.
type StatisticsPair struct {
	Home *int `xml:"home,attr,omitempty"`
	Away *int `xml:"away,attr,omitempty"`
}

func (p *StatisticsPair) ResolveHome() *int {
	if p == nil {
		return nil
	}
	return p.Home
}

func (p *StatisticsPair) ResolveAway() *int {
	if p == nil {
		return nil
	}
	return p.Away
}

type Statistics struct {
	YellowCards    *StatisticsPair `xml:"yellow_cards,omitempty"`
	RedCards       *StatisticsPair `xml:"red_cards,omitempty"`
	YellowRedCards *StatisticsPair `xml:"yellow_red_cards,omitempty"`
	Corners        *StatisticsPair `xml:"corners,omitempty"`
}
