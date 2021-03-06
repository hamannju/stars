package starmanager

import (
	"context"
	"errors"
	"github.com/asdine/storm"
	"github.com/asdine/storm/q"
	"github.com/gkze/stars/auth"
	"github.com/gkze/stars/utils"
	"github.com/google/go-github/v25/github"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/afero"
	"golang.org/x/oauth2"
	"math/rand"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	// GitHub - the GitHub API host
	GitHub string = "api.github.com"

	// CachePath - the path to the cache db file
	CachePath string = ".cache"

	// CacheFile - the filename of the db cache
	CacheFile string = "stars.db"

	// PageSize - the default response page size (GitHub maximum is 100 so we use that)
	PageSize int = 100
)

// Star represents the starred project that is saved locally
type Star struct {
	PushedAt    time.Time `storm:"index"`
	URL         string    `storm:"id,index,unique"`
	Language    string    `storm:"index"`
	Stargazers  int
	Archived    bool     `storm:"index"`
	Description string   `storm:"index"`
	Topics      []string `storm:"index"`
}

// StarManager is the central object used to manage stars for a GitHub account
type StarManager struct {
	Username string
	Password string
	Context  context.Context
	Client   *github.Client
	DB       *storm.DB
}

// New - initialize a new starmanager
func New() (*StarManager, error) {
	cfg, err := auth.NewConfig()
	if err != nil {
		return nil, err
	}

	netrcAuth, err := auth.NewNetrc(cfg)
	username, password, err := netrcAuth.GetAuth(GitHub)
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	client := github.NewClient(oauth2.NewClient(
		ctx, oauth2.StaticTokenSource(&oauth2.Token{AccessToken: password}),
	))

	currentUser, err := user.Current()
	if err != nil {
		log.Printf("Could not determine the current user! %v", err.Error())

		return nil, err
	}

	cacheDir := filepath.Join(currentUser.HomeDir, CachePath)
	cacheFullPath := filepath.Join(cacheDir, CacheFile)
	toCreate := []struct {
		path string
		mode os.FileMode
	}{
		{cacheDir, os.ModeDir},
		{cacheFullPath, 0},
	}

	for _, p := range toCreate {
		err := utils.CreateIfNotExists(p.path, p.mode, afero.NewOsFs())
		if err != nil {
			log.Printf("An error occurred while attempting to create %s: %v", p.path, err.Error())
		}
	}

	db, err := storm.Open(cacheFullPath, storm.Batch())
	if err != nil {
		log.Printf("An error occurred opening the db! %v", err.Error())

		return nil, err
	}

	return &StarManager{
		Username: username,
		Password: password,
		Context:  ctx,
		Client:   client,
		DB:       db,
	}, nil
}

// ClearCache resets the local db.
func (s *StarManager) ClearCache() error {
	if err := os.Remove(s.DB.Bolt.Path()); err != nil {
		return err
	}

	log.Printf("Cleared cache")
	return nil
}

// SaveStarredRepository saves a single starred project to the local cache.
func (s *StarManager) SaveStarredRepository(repo *github.Repository, wg *sync.WaitGroup) error {
	wg.Add(1)
	defer wg.Done()
	lang, desc := "", ""

	// We have to perform the below two checks because some repos don't have languages or
	// desciptions, and the client does not create those struct fields, resulting in a SIGSEGV
	// (segmentation fault).
	if repo.Language != nil {
		lang = *repo.Language
	}

	if repo.Description != nil {
		desc = *repo.Description
	}

	err := s.DB.Save(&Star{
		PushedAt:    repo.PushedAt.Time,
		URL:         *repo.HTMLURL,
		Language:    strings.ToLower(lang),
		Stargazers:  *repo.StargazersCount,
		Description: desc,
		Topics:      repo.Topics,
		Archived:    *repo.Archived,
	})
	if err != nil {
		return err
	}

	log.Printf("Saved %s (with topics %s)\n", *repo.HTMLURL, repo.Topics)
	return nil
}

// SaveStarredPage saves an entire page of starred repositories concurrently, optionally sending
// server responses to a channel if it is provided.
func (s *StarManager) SaveStarredPage(pageno int, responses chan *github.Response, wg *sync.WaitGroup) chan error {
	wg.Add(1)
	defer wg.Done()
	errors := make(chan error)

	firstPage, response, err := s.Client.Activity.ListStarred(
		s.Context,
		s.Username,
		&github.ActivityListStarredOptions{
			ListOptions: github.ListOptions{
				PerPage: PageSize,
				Page:    pageno,
			},
		},
	)
	if err != nil {
		log.Printf(
			"An error occurred while attempting to fetch page %d of %s's GitHub stars!",
			pageno,
			s.Username,
		)

		errors <- err
	}

	if responses != nil {
		responses <- response
	}

	log.Printf("Attempting to save starred projects on page %d...\n", pageno)
	for _, r := range firstPage {
		go s.SaveStarredRepository(r.Repository, wg)
	}

	return errors
}

// SaveAllStars saves all stars.
func (s *StarManager) SaveAllStars() (bool, error) {
	wg := sync.WaitGroup{}
	responses := make(chan *github.Response, 1)

	// Fetch the first page to determine the last page number from the response "Link" header
	log.Printf("Attempting to save first page...")
	go s.SaveStarredPage(1, responses, &wg)
	firstPageResponse := <-responses

	log.Printf("Attempting to save the rest of the pages...")
	for i := 2; i <= firstPageResponse.LastPage; i++ {
		go s.SaveStarredPage(i, nil, &wg)
	}
	wg.Wait()

	log.Printf("Successfully saved all starred projects")
	return true, nil
}

// SaveIfEmpty saves all stars if the local cache is empty
func (s *StarManager) SaveIfEmpty() error {
	if count, _ := s.DB.Count(&Star{}); count == 0 {
		if _, err := s.SaveAllStars(); err != nil {
			return err
		}
	}

	return nil
}

// KV is a generic struct that maintains a string key - int value pair ( :( ).
type KV struct {
	Key   string
	Value int
}

// GetTopics returns topics for a repository, otherwise if no repository is passed, returns
// a list of all topics
func (s *StarManager) GetTopics() []KV {
	stars := []Star{}
	topicCounts := map[string]int{}

	s.DB.All(&stars)

	for _, star := range stars {
		for _, topic := range star.Topics {
			topicCounts[topic]++
		}
	}

	results := []KV{}

	for topic, count := range topicCounts {
		results = append(results, KV{topic, count})
	}

	sort.Slice(results, func(i, j int) bool {
		return results[i].Value > results[j].Value
	})

	return results
}

// GetProjects returns random projects given a project count to return, and an optional
// language and topic to filter by.
func (s *StarManager) GetProjects(count int, language, topic string, random bool) ([]Star, error) {
	stars := []Star{}

	if language != "" {
		if err := s.DB.Select(q.Eq("Language", language)).Find(&stars); err != nil {
			return nil, err
		}
	} else {
		if err := s.DB.All(&stars); err != nil {
			return nil, err
		}
	}

	if topic != "" {
		topicStars := []Star{}

		for _, star := range stars {
			if utils.StringInSlice(topic, star.Topics) {
				topicStars = append(topicStars, star)
			}
		}

		stars = topicStars
	}

	if random == true {
		rand.Seed(time.Now().UTC().UnixNano())
		rand.Shuffle(len(stars), func(i, j int) {
			stars[i], stars[j] = stars[j], stars[i]
		})
	} else {
		sort.Slice(stars, func(i, j int) bool { return stars[i].Stargazers > stars[j].Stargazers })
	}

	if len(stars) > 0 {
		if len(stars) > count {
			return stars[0:count], nil
		}

		return stars, nil
	}

	return []Star{}, errors.New("No stars matching criteria found")
}

// RemoveStar unstars the project on Github and removes the star from the local cache.
func (s *StarManager) RemoveStar(star *Star, wg *sync.WaitGroup) (bool, error) {
	wg.Add(1)
	defer wg.Done()

	starURL, parseErr := url.Parse(star.URL)
	if parseErr != nil {
		return false, parseErr
	}

	splitPath := strings.Split(starURL.Path, "/")

	_, unstarErr := s.Client.Activity.Unstar(s.Context, splitPath[1], splitPath[2])
	if unstarErr != nil {
		log.Printf("An error occurred while attempting to unstar %s: %s\n", star.URL, unstarErr.Error())
		return false, unstarErr
	}

	deleteErr := s.DB.DeleteStruct(star)
	if deleteErr != nil {
		return false, deleteErr
	}

	log.Printf("Removed %s", star.URL)

	return true, nil
}

// Cleanup removes stars older than a specified time in months and optionally archived stars.
func (s *StarManager) Cleanup(age int, archived bool) error {
	allStars := []*Star{}
	toDelete := make(chan *Star)
	wg := sync.WaitGroup{}
	then := time.Now().AddDate(0, -age, 0)

	if err := s.DB.All(&allStars); err != nil {
		return err
	}

	log.Printf("Filtering stars to delete (from %d)...", len(allStars))
	for _, star := range allStars {
		if star.PushedAt.Before(then) || star.Archived == archived {
			log.Printf(
				"Queueing %s for deletion (last pushed at %+v, archive status: %t)",
				star.URL,
				star.PushedAt,
				star.Archived,
			)

			go func(ch chan *Star, s *Star, wg *sync.WaitGroup) {
				wg.Add(1)
				defer wg.Done()

				ch <- s
			}(toDelete, star, &wg)
		}
	}

	// Cannot close channel in main goroutine as it will block
	go func() {
		wg.Wait()
		close(toDelete)
	}()

	for star := range toDelete {
		go s.RemoveStar(star, &wg)
	}
	wg.Wait()

	return nil
}
