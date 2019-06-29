package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/MemeLabs/dggchat"
	"github.com/SoMuchForSubtlety/fileupload"
	"github.com/SoMuchForSubtlety/opendj"
	"github.com/Syfaro/haste-client"
	"google.golang.org/api/googleapi/transport"
	"google.golang.org/api/youtube/v3"
)

type config struct {
	AuthToken  string   `json:"auth_token"`
	Address    string   `json:"address"`
	Rtmp       string   `json:"rtmp"`
	APIKey     string   `json:"api_key"`
	Moderators []string `json:"moderators"`
}

type localQueue struct {
	Q []opendj.QueueEntry
}

type controller struct {
	ytServ    *youtube.Service
	cfg       config
	sgg       *dggchat.Session
	msgBuffer chan outgoingMessage
	dj        *opendj.Dj

	haste *haste.Haste

	playlistLink  string
	playlistDirty bool

	likes             userList
	updateSubscribers userList
}

type outgoingMessage struct {
	nick    string
	message string
}

func main() {
	cont, err := initController()
	if err != nil {
		log.Fatalf("[ERROR] could not initialize controller: %v", err)
	}

	// Open a connection
	err = cont.sgg.Open()
	if err != nil {
		log.Fatalln(err)
	}
	// Cleanly close the connection
	defer cont.sgg.Close()

	go cont.dj.Play(cont.cfg.Rtmp)

	cont.messageSender()
	// Wait for ctr-C to shut down
	sc := make(chan os.Signal, 1)
	signal.Notify(sc, syscall.SIGINT)
	<-sc
}

func initController() (c *controller, err error) {
	var cont controller

	cont.playlistDirty = true

	cont.cfg, err = readConfig("newConfig.json")
	if err != nil {
		return nil, err
	}

	cont.msgBuffer = make(chan outgoingMessage, 100)
	cont.haste = haste.NewHaste("https://hastebin.com")

	client := &http.Client{Transport: &transport.APIKey{Key: cont.cfg.APIKey}}
	cont.ytServ, err = youtube.New(client)
	if err != nil {
		return nil, err
	}

	// load the saved playlist if there is one
	var queue localQueue

	file, err := ioutil.ReadFile("queue.json")
	if err != nil {
		log.Printf("[INFO] no previous playlist found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &queue)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal queue: %v", err)
		} else {
			log.Printf("[INFO] loaded playlist with %v songs", len(queue.Q))
		}
	}

	// load update subscribers
	file, err = ioutil.ReadFile("updateUsers.json")
	if err != nil {
		log.Printf("[INFO] no user list found: %v", err)
	} else {
		err = json.Unmarshal([]byte(file), &cont.updateSubscribers)
		if err != nil {
			log.Printf("[ERROR] failed to unmarshal user list: %v", err)
		} else {
			log.Printf("[INFO] loaded user list with %v entries", len(cont.updateSubscribers.Users))
		}
	}

	// create dj
	cont.dj, err = opendj.NewDj(queue.Q)
	if err != nil {
		return nil, err
	}

	cont.dj.AddNewSongHandler(cont.newSong)
	cont.dj.AddEndOfSongHandler(cont.songOver)
	cont.dj.AddPlaybackErrorHandler(cont.songError)

	// Create a new sgg client
	cont.sgg, err = dggchat.New(";jwt=" + cont.cfg.AuthToken)
	u, err := url.Parse(cont.cfg.Address)
	if err != nil {
		log.Fatalf("[ERROR] can't parse url %v", err)
	}
	cont.sgg.SetURL(*u)

	if err != nil {
		return nil, err
	}

	cont.sgg.AddPMHandler(cont.onPrivMessage)
	cont.sgg.AddErrorHandler(onError)

	return &cont, nil
}

func readConfig(title string) (cfg config, err error) {
	file, err := os.Open(title)
	if err != nil {
		return cfg, err
	}
	defer file.Close()

	bv, err := ioutil.ReadAll(file)
	if err != nil {
		return cfg, err
	}

	err = json.Unmarshal(bv, &cfg)
	return cfg, err
}

func (c *controller) onPrivMessage(m dggchat.PrivateMessage, s *dggchat.Session) {
	log.Printf("New message from %s: %s\n", m.User, m.Message)

	trimmedMsg := strings.TrimSpace(m.Message)

	ytURL := regexp.MustCompile(`(youtube.com\/watch\?v=|youtu.be\/)[a-zA-Z0-9_-]+`)

	if ytURL.Match([]byte(trimmedMsg)) {
		c.addYTlink(m)
	}
	switch trimmedMsg {
	case "-playing":
		c.sendCurrentSong(m.User.Nick)
		return
	case "-next":
		c.sendNextSong(m.User.Nick)
		return
	case "-queue":
		c.sendQueuePositions(m.User.Nick)
		return
	case "-playlist":
		c.sendPlaylist(m.User.Nick)
		return
	case "-updateme":
		c.addUserToUpdates(m.User.Nick)
		return
	case "-like":
		c.likeSong(m.Message)
	default:
	}

	if strings.Contains(trimmedMsg, "-remove") {
		c.removeItem(trimmedMsg, m.User.Nick)
		return
	} else if strings.Contains(trimmedMsg, "-dedicate") {
		c.addDedication(m.Message, m.User.Nick)
	}
}

func onError(e string, s *dggchat.Session) {
	log.Printf("[ERROR] error from ws: %s", e)
}

func (c *controller) addYTlink(m dggchat.PrivateMessage) {
	queue := c.dj.Queue()
	var duration time.Duration
	var maxduration float64

	for _, item := range queue {
		duration += item.Media.Duration
	}
	item, progress, err := c.dj.CurrentlyPlaying()
	if err == nil {
		duration += item.Media.Duration - progress
	}

	if duration.Minutes() <= 1 {
		maxduration = 60
	} else if duration.Minutes() <= 20 {
		maxduration = 20
	} else if duration.Minutes() <= 60 {
		maxduration = 10
	} else {
		maxduration = 5
	}

	ytURLStart := "https://www.youtube.com/watch?v="

	id := regexp.MustCompile(`(\?v=|be\/)[a-zA-Z0-9-_]+`).FindString(m.Message)[3:]
	if id == "" {
		c.sendMsg("invalid link", m.User.Nick)
		return
	}

	res, err := c.ytServ.Videos.List("id,snippet,contentDetails").Id(id).Do()
	songDuration, _ := time.ParseDuration(strings.ToLower(res.Items[0].ContentDetails.Duration[2:]))
	if err != nil {
		log.Printf("[ERROR] youtube API query failed: %v", err)
		c.sendMsg("there was an error", m.User.Nick)
		return
	} else if len(res.Items) < 1 {
		c.sendMsg("invalid link", m.User.Nick)
		return
	} else if songDuration.Minutes() >= maxduration {
		c.sendMsg(fmt.Sprintf("This song is too long, please keep it under %v minutes", maxduration), m.User.Nick)
		return
	}

	var video opendj.Media
	video.Title = res.Items[0].Snippet.Title
	video.Duration = songDuration
	video.URL = ytURLStart + res.Items[0].Id

	var entry opendj.QueueEntry
	entry.Media = video
	entry.Owner = m.User.Nick

	c.dj.AddEntry(entry)
	q := localQueue{Q: c.dj.Queue()}
	saveStruct(q, "queue.json")
	c.playlistDirty = true

	c.sendMsg(fmt.Sprintf("Added your request '%v' to the queue.", entry.Media.Title), m.User.Nick)
	log.Printf("Added song: '%v' for %v", entry.Media.Title, entry.Owner)
}

func (c *controller) sendCurrentSong(nick string) {
	song, elapsed, err := c.dj.CurrentlyPlaying()
	video := song.Media
	if err != nil {
		c.sendMsg("there is nothing playing right now :(", nick)
		return
	}
	response := fmt.Sprintf("`%v` `%v/%v` currently playing: 🎶 %q 🎶 requested by %s", durationBar(15, elapsed, video.Duration), fmtDuration(elapsed), fmtDuration(video.Duration), video.Title, song.Owner)
	c.sendMsg(response, nick)
}

func (c *controller) sendNextSong(nick string) {
	queue := c.dj.Queue()
	if len(queue) <= 0 {
		c.sendMsg("there is nothing in the queue :(", nick)
		return
	}

	c.sendMsg(fmt.Sprintf("up next: '%v' requested by %s", queue[0].Media.Title, queue[0].Owner), nick)
}

func (c *controller) sendQueuePositions(nick string) {
	queue := c.dj.Queue()
	positions := c.dj.UserPosition(nick)
	durations := c.dj.DurationUntilUser(nick)
	response := fmt.Sprintf("There are currently %v songs in the queue", len(queue))
	if len(positions) > 0 {
		if len(positions) != len(durations) {
			c.sendMsg("there was an error", nick)
			return
		}
		for i, duration := range durations {
			response += fmt.Sprintf(", your song is in position %v and will play in %v", i+1, fmtDuration(duration))
		}
	}
	c.sendMsg(response, nick)
}

func (c *controller) sendPlaylist(nick string) {
	if c.playlistDirty {
		currentSong, _, _ := c.dj.CurrentlyPlaying()
		playlist := formatPlaylist(c.dj.Queue(), currentSong)

		url, err := c.uploadString(playlist)
		if err != nil {
			log.Printf("[ERROR] failed to upload playlist: %v", err)
			c.sendMsg("there was an error", nick)
			return
		}

		log.Println("[INFO] 📝 Generated playlist")
		c.playlistLink = url
		c.playlistDirty = false
	}

	c.sendMsg(fmt.Sprintf("you can find the current playlist here: %v", c.playlistLink), nick)
}

func (c *controller) likeSong(nick string) {
	playing, _, err := c.dj.CurrentlyPlaying()
	if err != nil {
		c.sendMsg("There is nothing currently playing.", nick)
		return
	}
	result := c.likes.search(nick)
	if result > 0 {
		c.sendMsg("You already liked this song.", nick)
		return
	}
	c.likes.add(nick)
	c.sendMsg(fmt.Sprintf("I will tell %v you liked \"%v\"", playing.Owner, playing.Media.Title), nick)
}

func (c *controller) addUserToUpdates(nick string) {
	index := c.updateSubscribers.search(nick)
	if index < 0 {
		c.sendMsg("You will now get a message every time a new song plays. send `-updateme` again to turn it off.", nick)
		c.updateSubscribers.add(nick)
	} else {
		c.sendMsg("You will no longer get notifications.", nick)
		c.updateSubscribers.remove(nick)
	}
	saveStruct(&c.updateSubscribers, "updateUsers.json")
}

func (c *controller) removeItem(message string, nick string) {
	intString := strings.TrimSpace(strings.Replace(message, "-remove", "", -1))
	index, err := strconv.Atoi(intString)
	if err != nil {
		c.sendMsg("please enter a valid integer", nick)
		return
	}

	entry, err := c.dj.EntryAtIndex(index)
	if err != nil {
		c.sendMsg("Index out of range", nick)
		return
	}

	if nick != entry.Owner || !c.isMod(nick) {
		c.sendMsg(fmt.Sprintf("I can't allow you to do that, %v", nick), nick)
		return
	}

	err = c.dj.RemoveIndex(index)
	if err != nil {
		c.sendMsg("index out of range", nick)
		return
	}
	queue := localQueue{Q: c.dj.Queue()}
	saveStruct(queue, "queue.json")
	c.playlistDirty = true
	c.sendMsg("Successfully removed item at index", nick)

	return
}

func (c *controller) addDedication(message string, nick string) {
	positions := c.dj.UserPosition(nick)
	if len(positions) <= 0 {
		c.sendMsg("you have no songs in the queue", nick)
		return
	}

	dedication := strings.TrimSpace(strings.Replace(message, "-dedicate", "", -1))

	entry, err := c.dj.EntryAtIndex(positions[0])
	if err != nil {
		c.sendMsg("there was an error", nick)
		return
	}
	entry.Dedication = dedication
	err = c.dj.ChangeIndex(entry, positions[0])
	if err != nil {
		c.sendMsg("there was an error", nick)
		return
	}

	c.sendMsg(fmt.Sprintf("Dedicated %v to %v", entry.Media.Title, dedication), nick)
}

func (c *controller) isMod(nick string) bool {
	for _, mod := range c.cfg.Moderators {
		if nick == mod {
			return true
		}
	}
	return false
}

func (c *controller) uploadString(text string) (url string, err error) {
	var file *os.File

	type resp struct {
		response *haste.Response
		er       error
	}

	c1 := make(chan resp)
	go func() {
		hasteResp, err := c.haste.UploadString(text)
		c1 <- resp{response: hasteResp, er: err}
	}()

	select {
	case res := <-c1:
		err = res.er
		url = "https://hastebin.com/raw/" + res.response.Key
	case <-time.After(1 * time.Second):
		// TODO: find a better way to do this
		err = ioutil.WriteFile("tmp.txt", []byte(text), 0644)
		if err != nil {
			return url, err
		}
		file, err = os.Open("tmp.txt")
		if err != nil {
			return url, err
		}
		url, err = fileupload.UploadToHost("https://uguu.se/api.php?d=upload-tool", file)
	}

	return url, err
}

func (c *controller) sendMsg(message string, nick string) {
	if _, inChat := c.sgg.GetUser(nick); inChat {
		c.msgBuffer <- outgoingMessage{nick: nick, message: message}
	}
}

func (c *controller) messageSender() {
	for {
		// TODO: verify the message was sent
		msg := <-c.msgBuffer
		c.sgg.SendPrivateMessage(msg.nick, msg.message)
		log.Printf("[MSG] message sent to %v: %v", msg.nick, msg.message)
		time.Sleep(time.Millisecond * 450)
	}
}

func (c *controller) newSong(entry opendj.QueueEntry) {
	c.playlistDirty = true
	msg := fmt.Sprintf("Now Playing %s's request: %s", entry.Owner, entry.Media.Title)
	log.Println("[INFO] ▶ " + msg)

	c.updateSubscribers.Lock()
	for _, user := range c.updateSubscribers.Users {
		c.sendMsg(msg, user)
	}
	c.updateSubscribers.Unlock()

	if entry.Dedication != "" {
		c.sendMsg(fmt.Sprintf("%s dedicated this song to you.", entry.Owner), entry.Dedication)
	}

	c.sendMsg("Playing your song now", entry.Owner)
}

func (c *controller) songOver(entry opendj.QueueEntry, err error) {
	c.playlistDirty = true
	log.Println("[INFO] 🛑 Done Playing")
	queue := localQueue{Q: c.dj.Queue()}
	saveStruct(queue, "queue.json")

	likes := len(c.likes.Users)
	if likes > 0 {
		ppl := "people"
		if likes == 1 {
			ppl = "person"
		}
		c.sendMsg(fmt.Sprintf("%v %v really liked your song PeepoHappy", likes, ppl), entry.Owner)
	}
	c.likes.clear()
}

func (c *controller) songError(err error) {
	log.Printf("[ERROR] there was an error during song playback: %v", err)
}

func saveStruct(v interface{}, title string) error {
	file, err := json.MarshalIndent(&v, "", "	")
	if err != nil {
		return err
	}
	err = ioutil.WriteFile(title, file, 0644)
	if err != nil {
		return err
	}
	return nil
}
