package xml

type StatisticsPair struct {
	Home *uint32 `xml:"home,attr,omitempty"`
	Away *uint32 `xml:"away,attr,omitempty"`
}

type Statistics struct {
	YellowCards    *StatisticsPair `xml:"yellow_cards,omitempty"`
	RedCards       *StatisticsPair `xml:"red_cards,omitempty"`
	YellowRedCards *StatisticsPair `xml:"yellow_red_cards,omitempty"`
	Corners        *StatisticsPair `xml:"corners,omitempty"`
}
