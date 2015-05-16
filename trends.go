package anaconda

import (
	"net/url"
)

type Location struct {
	Country     string `json:"country,omitempty"`
	CountryCode string `json:"countryCode,omitempty"`
	Name        string `json:"name"`
	ParentId    int64  `json:"parentid,omitempty"`
	PlaceType   struct {
		Code int64  `json:"code"`
		Name string `json:"name"`
	} `json:"placeType,,omitempty"`
	url   string `json:"url,omitempty"`
	WoeId int64  `json:"woeid"`
}

type TrendResponse struct {
	AsOf      string     `json:"as_of"`
	CreatedAt string     `json:"created_at"`
	Locations []Location `json:"locations"`
	Trends    []Trend    `json:"trends"`
}

type Trend struct {
	Events          string `json:"events,omitempty"`
	Name            string `json:"name"`
	PromotedContent string `json:"promoted_content,omitempty"`
	Query           string `json:"query"`
	Url             string `json:"url"`
}

// GetPlaceTrends implements /trends/place.json
func (a TwitterApi) GetPlaceTrends(id string, exclude string, v url.Values) (trendResponse TrendResponse, err error) {
	if v == nil {
		v = url.Values{}
	}
	v.Set("id", id)
	v.Set("exclude", exclude)

	response_ch := make(chan response)
	a.queryQueue <- query{BaseUrl + "/trends/place.json", v, &trendResponse, _GET, response_ch}
	return trendResponse, (<-response_ch).err
}

// GetAvailableTrends implements /trends/available.json
func (a TwitterApi) GetAvailableTrends(v url.Values) (locations []Location, err error) {
	if v == nil {
		v = url.Values{}
	}

	response_ch := make(chan response)
	a.queryQueue <- query{BaseUrl + "/trends/available.json", v, &locations, _GET, response_ch}
	return locations, (<-response_ch).err
}

// GetClosestTrends implements /trends/closest.json
func (a TwitterApi) GetClosestTrends(lat string, long string, v url.Values) (location Location, err error) {
	if v == nil {
		v = url.Values{}
	}
	v.Set("lat", lat)
	v.Set("long", long)

	response_ch := make(chan response)
	a.queryQueue <- query{BaseUrl + "/trends/closest.json", v, &location, _GET, response_ch}
	return location, (<-response_ch).err
}
