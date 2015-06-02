package anaconda

import (
	"bufio"
	"encoding/json"
	"fmt"
	redis "github.com/alphazero/go-redis"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/dustin/go-jsonpointer"
)

const (
	BaseUrlUserStream = "https://userstream.twitter.com/1.1"
	BaseUrlSiteStream = "https://sitestream.twitter.com/1.1"
	BaseUrlStream     = "https://stream.twitter.com/1.1"
)

// messages

type StatusDeletionNotice struct {
	Id        int64  `json:"id"`
	IdStr     string `json:"id_str"`
	UserId    int64  `json:"user_id"`
	UserIdStr string `json:"user_id_str"`
}
type statusDeletionNotice struct {
	Delete *struct {
		Status *StatusDeletionNotice `json:"status"`
	} `json:"delete"`
}

type LocationDeletionNotice struct {
	UserId          int64  `json:"user_id"`
	UserIdStr       string `json:"user_id_str"`
	UpToStatusId    int64  `json:"up_to_status_id"`
	UpToStatusIdStr string `json:"up_to_status_id_str"`
}
type locationDeletionNotice struct {
	ScrubGeo *LocationDeletionNotice `json:"scrub_geo"`
}

type LimitNotice struct {
	Track int64 `json:"track"`
}
type limitNotice struct {
	Limit *LimitNotice `json:"limit"`
}

type StatusWithheldNotice struct {
	Id                  int64    `json:"id"`
	UserId              int64    `json:"user_id"`
	WithheldInCountries []string `json:"withheld_in_countries"`
}
type statusWithheldNotice struct {
	StatusWithheld *StatusWithheldNotice `json:"status_withheld"`
}

type UserWithheldNotice struct {
	Id                  int64    `json:"id"`
	WithheldInCountries []string `json:"withheld_in_countries"`
}
type userWithheldNotice struct {
	UserWithheld *UserWithheldNotice `json:"user_withheld"`
}

type DisconnectMessage struct {
	Code       int64  `json:"code"`
	StreamName string `json:"stream_name"`
	Reason     string `json:"reason"`
}
type disconnectMessage struct {
	Disconnect *DisconnectMessage `json:"disconnect"`
}

type StallWarning struct {
	Code        string `json:"code"`
	Message     string `json:"message"`
	PercentFull int64  `json:"percent_full"`
}
type stallWarning struct {
	Warning *StallWarning `json:"warning"`
}

type FriendsList []int64
type friendsList struct {
	Friends *FriendsList `json:"friends"`
}

type streamDirectMessage struct {
	DirectMessage *DirectMessage `json:"direct_message"`
}

type Event struct {
	Target    *User  `json:"target"`
	Source    *User  `json:"source"`
	Event     string `json:"event"`
	CreatedAt string `json:"created_at"`
}

type EventList struct {
	Event
	TargetObject *List `json:"target_object"`
}

type EventTweet struct {
	Event
	TargetObject *Tweet `json:"target_object"`
}

type TooManyFollow struct {
	Warning *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		UserId  int64  `json:"user_id"`
	} `json:"warning"`
}

// TODO: Site Stream messages. I cant test.

// TODO: May be we could pass it a Logger interface to allow the
// stream to log in the right place ?

// Stream allows you to stream using one of the
// PublicStream* or UserStream api methods
//
// A go loop is started an gives you an interface{}
// Which you can cast into a tweet like this :
//    t, ok := o.(twitter.Tweet) // try casting into a tweet
//    if !ok {
//      log.Debug("Recieved non tweet message")
//    }
//
// If we can't stream the chan will be closed.
// Otherwise the loop will connect and send streams in the chan.
// It will also try to reconnect itself after 2s if the connection is lost
// If twitter response is one of 420, 429 or 503 (meaning "wait a sec")
// the loop retries to open the socket with a simple autogrowing backoff.
//
// When finished you can call stream.Close() to terminate remote connection.
//

type Stream struct {
	api  TwitterApi
	C    chan Tweet //Where outputted tweets go
	Quit chan bool

	streamBody  *io.ReadCloser
	waitGroup   *sync.WaitGroup
	redisClient *redis.Client
	meter       FlowMeter //used to prevent stalls/monitor flow
}

// Measure flow of tweets
type FlowMeter struct {
	//Used to prevent twitter stalls
	ticker90 chan time.Time
	count90  int

	//Used for monitoring flow
	ticker60 chan time.Time
	count60  int

	//Are we logging flow rate to redis?
	redisFlow bool

	//Used if redis logging enabled
	keyPrefix string
}

func (s Stream) resetTicker90() {
	for _ = range s.meter.ticker90 {
		if s.meter.count90 == 0 {
			log.Printf("%s recieved no tweets for 90 seconds. Reconnecting...\n", s.meter.keyPrefix)
			(*s.streamBody).Close()
		}
		s.meter.count90 = 0
	}
}

func (s Stream) resetTicker60() {
	for _ = range s.meter.ticker60 {
		(*s.redisClient).Set(
			fmt.Sprintf("%s$last60s", s.meter.keyPrefix),
			[]byte(strconv.FormatInt(int64(s.meter.count60), 10)),
		)
		s.meter.count60 = 0
	}
}

func (s Stream) initFlowMeter(keyPrefix string) {
	m := FlowMeter{}
	if s.redisClient != nil {
		m.redisFlow = true
		go s.resetTicker60()
	}
	s.meter = m
	s.meter.keyPrefix = keyPrefix
	go s.resetTicker90()
}

//Worker keeps an accurate flow rate
func (s Stream) Push(t Tweet) {
	s.C <- t
	s.meter.count90++
	if s.meter.redisFlow {
		s.meter.count60++
	}
}

// Interrupt starts the finishing sequence
func (s Stream) Interrupt() {
	s.api.Log.Notice("Stream closing...")
	close(s.meter.ticker90)
	if s.meter.redisFlow {
		close(s.meter.ticker60)
	}
	close(s.Quit)
	s.api.Log.Debug("Stream closed.")
}

//End wait for closability
func (s Stream) End() {
	s.waitGroup.Wait()
	close(s.C)
}

func (s Stream) listen() {
	defer (*s.streamBody).Close()
	log.Printf("%s: Now listening to twitter socket\n", s.api.ScreenName)
	reader := bufio.NewReaderSize(*s.streamBody, 1000000)
	for {
		var line []byte
		var err error
		if line, err = reader.ReadBytes('\n'); err != nil {
			if len(line) > 1 {
				log.Printf("%s received corrupted message Continuing. Error: %s\n", s.api.ScreenName, err.Error())
				continue
			} else {
				log.Printf("%s received no message. Reconnecting. Error: %s\n", s.api.ScreenName, err.Error())
				return
			}
		}
		select {
		case <-s.Quit:
			s.api.Log.Debug("leaving response loop")
			return
		default:
			// TODO: DRY
			if string(line) == "" {
				log.Printf("%s: empty bytes received\n", s.api.ScreenName)
				continue
			} else if o := new(Tweet); jsonAsStruct(line, "/source", o) {
				s.api.Log.Debug("Got a Tweet")
				s.C <- *o
			} else if o := new(statusDeletionNotice); jsonAsStruct(line, "/delete", o) {
				log.Printf("%s: status deletion received\n", s.api.ScreenName)
			} else if o := new(locationDeletionNotice); jsonAsStruct(line, "/scrub_geo", o) {
				log.Printf("%s: location deletion received\n", s.api.ScreenName)
			} else if o := new(limitNotice); jsonAsStruct(line, "/limit", o) {
				log.Printf("%s Got a stream limit notice: %s\n", s.api.ScreenName, string(line))
			} else if o := new(statusWithheldNotice); jsonAsStruct(line, "/status_withheld", o) {
				log.Printf("%s Got a status withheld notice: %s\n", s.api.ScreenName, string(line))
			} else if o := new(userWithheldNotice); jsonAsStruct(line, "/user_withheld", o) {
				log.Printf("%s Got a user withheld notice: %s\n", s.api.ScreenName, string(line))
			} else if o := new(disconnectMessage); jsonAsStruct(line, "/disconnect", o) {
				log.Printf("%s got a disconnectMessage. Reconnecting... %s\n", s.api.ScreenName, string(line))
				return
			} else if o := new(stallWarning); jsonAsStruct(line, "/warning", o) {
				log.Printf("%s got a stallWarning: %s\n", s.api.ScreenName, string(line))
			} else if o := new(friendsList); jsonAsStruct(line, "/friends", o) {
				s.api.Log.Debug("Got a friendsList")
			} else if o := new(streamDirectMessage); jsonAsStruct(line, "/direct_message", o) {
				s.api.Log.Debug("Got a streamDirectMessage")
			} else if o := new(EventTweet); jsonAsStruct(line, "/target_object/source", o) {
				s.api.Log.Debug("Got a EventTweet")
			} else if o := new(Event); jsonAsStruct(line, "/target_object", o) {
				s.api.Log.Debug("Got a Event")
			} else {
			}
		}
	}
}

func (s Stream) requestStream(a *TwitterApi, urlStr string, v url.Values, method int) (resp *http.Response, err error) {
	switch method {
	case _GET:
		return a.OauthClient.Get(s.api.HttpClient, s.api.Credentials, urlStr, v)
	case _POST:
		return a.OauthClient.Post(s.api.HttpClient, s.api.Credentials, urlStr, v)
	default:
	}
	return nil, fmt.Errorf("HTTP method not yet supported")
}

func (s Stream) loop(a *TwitterApi, urlStr string, v url.Values, method int) {
	defer s.api.Log.Debug("Leaving request stream loop")
	defer s.waitGroup.Done()

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	baseBackoff := time.Duration(2 * time.Second)
	calmDownBackoff := time.Duration(10 * time.Second)
	backoff := baseBackoff
	for {
		select {
		case <-s.Quit:
			log.Printf("%s is leaving stream loop for some reason... this shouldn't happen\n", s.api.ScreenName)
			return
		default:
			resp, err := s.requestStream(a, urlStr, v, method)
			if err != nil {
				log.Printf("%s: Cannot request stream. Waiting 30 seconds... maybe shit's transient : %s", s.api.ScreenName, err)
				time.Sleep(30)
				continue
				// trigger quit but donnot close chan
			}

			switch resp.StatusCode {
			case 200, 304:
				s.streamBody = &resp.Body
				s.listen()
				backoff = baseBackoff
			case 420, 429, 503:
				log.Printf("%s: waiting %+s and backing off as we got: %+s\n", s.api.ScreenName, calmDownBackoff, resp.Status)
				time.Sleep(calmDownBackoff)
				backoff = baseBackoff + time.Duration(r.Int63n(10))
				s.api.Log.Debugf("backing off %s", backoff)
				time.Sleep(backoff)
			case 400, 401, 403, 404, 406, 410, 422, 500, 502, 504:
				log.Printf("%s: leaving after an irremediable error: %+s", s.api.ScreenName, resp.Status)
				s.Quit <- true
				// trigger quit but donnot close chan
				return
			default:
				s.api.Log.Notice("Received unknown status: %+s", resp.StatusCode)
			}

		}
	}
}

func (s Stream) Start(a *TwitterApi, urlStr string, v url.Values, method int) {
	s.waitGroup.Add(1)
	go s.loop(a, urlStr, v, method)
}

func (a TwitterApi) newStream(urlStr string, v url.Values, method int) Stream {
	stream := Stream{
		api:         a,
		Quit:        make(chan bool),
		C:           make(chan Tweet),
		waitGroup:   &sync.WaitGroup{},
		redisClient: a.RedisClient,
	}
	if stream.redisClient != nil {
		stream.initFlowMeter(a.ScreenName)
	}
	stream.Start(&a, urlStr, v, method)
	return stream
}

func (a TwitterApi) UserStream(v url.Values) (stream Stream) {
	return a.newStream(BaseUrlUserStream+"/user.json", v, _GET)
}

func (a TwitterApi) PublicStreamSample(v url.Values) (stream Stream) {
	return a.newStream(BaseUrlStream+"/statuses/sample.json", v, _GET)
}

// XXX: To use this API authority is requied. but I dont have this. I cant test.
func (a TwitterApi) PublicStreamFirehose(v url.Values) (stream Stream) {
	return a.newStream(BaseUrlStream+"/statuses/firehose.json", v, _GET)
}

// XXX: PublicStream(Track|Follow|Locations) func is needed?
func (a TwitterApi) PublicStreamFilter(v url.Values) (stream Stream) {
	return a.newStream(BaseUrlStream+"/statuses/filter.json", v, _POST)
}

// XXX: To use this API authority is requied. but I dont have this. I cant test.
func (a TwitterApi) SiteStream(v url.Values) (stream Stream) {
	return a.newStream(BaseUrlSiteStream+"/site.json", v, _GET)
}

func jsonAsStruct(j []byte, path string, obj interface{}) (res bool) {
	if v, _ := jsonpointer.Find(j, path); v == nil {
		return false
	}
	err := json.Unmarshal(j, obj)
	return err == nil
}
