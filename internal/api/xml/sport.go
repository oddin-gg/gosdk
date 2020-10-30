package xml

// Sport ...
type Sport struct {
	ID           string  `xml:"id,attr"`
	RefID        *string `xml:"ref_id,attr,omitempty"`
	Name         string  `xml:"name,attr"`
	Abbreviation string  `xml:"abbreviation,attr"`
	IconPath     *string  `xml:"icon_path,attr,omitempty"`
}
