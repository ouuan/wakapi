package imports

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"github.com/emvi/logbuch"
	"github.com/muety/wakapi/config"
	"github.com/muety/wakapi/models"
	wakatime "github.com/muety/wakapi/models/compat/wakatime/v1"
	"github.com/muety/wakapi/utils"
	"go.uber.org/atomic"
	"golang.org/x/sync/semaphore"
	"net/http"
	"time"
)

const OriginWakatime = "wakatime"
const maxWorkers = 6

type WakatimeHeartbeatImporter struct {
	ApiKey string
}

func NewWakatimeHeartbeatImporter(apiKey string) *WakatimeHeartbeatImporter {
	return &WakatimeHeartbeatImporter{
		ApiKey: apiKey,
	}
}

func (w *WakatimeHeartbeatImporter) Import(user *models.User, minFrom time.Time, maxTo time.Time) <-chan *models.Heartbeat {
	out := make(chan *models.Heartbeat)

	go func(user *models.User, out chan *models.Heartbeat) {
		startDate, endDate, err := w.fetchRange()
		if err != nil {
			logbuch.Error("failed to fetch date range while importing wakatime heartbeats for user '%s' – %v", user.ID, err)
			return
		}

		if startDate.Before(minFrom) {
			startDate = minFrom
		}
		if endDate.After(maxTo) {
			endDate = maxTo
		}

		userAgents, err := w.fetchUserAgents()
		if err != nil {
			logbuch.Error("failed to fetch user agents while importing wakatime heartbeats for user '%s' – %v", user.ID, err)
			return
		}

		machinesNames, err := w.fetchMachineNames()
		if err != nil {
			logbuch.Error("failed to fetch machine names while importing wakatime heartbeats for user '%s' – %v", user.ID, err)
			return
		}

		days := generateDays(startDate, endDate)

		c := atomic.NewUint32(uint32(len(days)))
		ctx := context.TODO()
		sem := semaphore.NewWeighted(maxWorkers)

		for _, d := range days {
			if err := sem.Acquire(ctx, 1); err != nil {
				logbuch.Error("failed to acquire semaphore – %v", err)
				break
			}

			go func(day time.Time) {
				defer sem.Release(1)

				d := day.Format("2006-01-02")
				heartbeats, err := w.fetchHeartbeats(d)
				if err != nil {
					logbuch.Error("failed to fetch heartbeats for day '%s' and user '%s' – &v", day, user.ID, err)
				}

				for _, h := range heartbeats {
					out <- mapHeartbeat(h, userAgents, machinesNames, user)
				}

				if c.Dec() == 0 {
					close(out)
				}
			}(d)
		}
	}(user, out)

	return out
}

func (w *WakatimeHeartbeatImporter) ImportAll(user *models.User) <-chan *models.Heartbeat {
	return w.Import(user, time.Time{}, time.Now())
}

// https://wakatime.com/api/v1/users/current/heartbeats?date=2021-02-05
func (w *WakatimeHeartbeatImporter) fetchHeartbeats(day string) ([]*wakatime.HeartbeatEntry, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest(http.MethodGet, config.WakatimeApiUrl+config.WakatimeApiHeartbeatsUrl, nil)
	if err != nil {
		return nil, err
	}

	q := req.URL.Query()
	q.Add("date", day)
	req.URL.RawQuery = q.Encode()

	res, err := httpClient.Do(w.withHeaders(req))
	if err != nil {
		return nil, err
	}

	var heartbeatsData wakatime.HeartbeatsViewModel
	if err := json.NewDecoder(res.Body).Decode(&heartbeatsData); err != nil {
		return nil, err
	}

	return heartbeatsData.Data, nil
}

// https://wakatime.com/api/v1/users/current/all_time_since_today
func (w *WakatimeHeartbeatImporter) fetchRange() (time.Time, time.Time, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	notime := time.Time{}

	req, err := http.NewRequest(http.MethodGet, config.WakatimeApiUrl+config.WakatimeApiAllTimeUrl, nil)
	if err != nil {
		return notime, notime, err
	}

	res, err := httpClient.Do(w.withHeaders(req))
	if err != nil {
		return notime, notime, err
	}

	var allTimeData wakatime.AllTimeViewModel
	if err := json.NewDecoder(res.Body).Decode(&allTimeData); err != nil {
		return notime, notime, err
	}

	startDate, err := time.Parse("2006-01-02", allTimeData.Data.Range.StartDate)
	if err != nil {
		return notime, notime, err
	}

	endDate, err := time.Parse("2006-01-02", allTimeData.Data.Range.EndDate)
	if err != nil {
		return notime, notime, err
	}

	return startDate, endDate, nil
}

// https://wakatime.com/api/v1/users/current/user_agents
func (w *WakatimeHeartbeatImporter) fetchUserAgents() (map[string]*wakatime.UserAgentEntry, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest(http.MethodGet, config.WakatimeApiUrl+config.WakatimeApiUserAgentsUrl, nil)
	if err != nil {
		return nil, err
	}

	res, err := httpClient.Do(w.withHeaders(req))
	if err != nil {
		return nil, err
	}

	var userAgentsData wakatime.UserAgentsViewModel
	if err := json.NewDecoder(res.Body).Decode(&userAgentsData); err != nil {
		return nil, err
	}

	userAgents := make(map[string]*wakatime.UserAgentEntry)
	for _, ua := range userAgentsData.Data {
		userAgents[ua.Id] = ua
	}

	return userAgents, nil
}

// https://wakatime.com/api/v1/users/current/machine_names
func (w *WakatimeHeartbeatImporter) fetchMachineNames() (map[string]*wakatime.MachineEntry, error) {
	httpClient := &http.Client{Timeout: 10 * time.Second}

	req, err := http.NewRequest(http.MethodGet, config.WakatimeApiUrl+config.WakatimeApiMachineNamesUrl, nil)
	if err != nil {
		return nil, err
	}

	res, err := httpClient.Do(w.withHeaders(req))
	if err != nil {
		return nil, err
	}

	var machineData wakatime.MachineViewModel
	if err := json.NewDecoder(res.Body).Decode(&machineData); err != nil {
		return nil, err
	}

	machines := make(map[string]*wakatime.MachineEntry)
	for _, ma := range machineData.Data {
		machines[ma.Id] = ma
	}

	return machines, nil
}

func (w *WakatimeHeartbeatImporter) withHeaders(req *http.Request) *http.Request {
	req.Header.Set("Authorization", fmt.Sprintf("Basic %s", base64.StdEncoding.EncodeToString([]byte(w.ApiKey))))
	return req
}

func mapHeartbeat(
	entry *wakatime.HeartbeatEntry,
	userAgents map[string]*wakatime.UserAgentEntry,
	machineNames map[string]*wakatime.MachineEntry,
	user *models.User,
) *models.Heartbeat {
	ua := userAgents[entry.UserAgentId]
	if ua == nil {
		ua = &wakatime.UserAgentEntry{
			Editor: "unknown",
			Os:     "unknown",
		}
	}

	ma := machineNames[entry.MachineNameId]
	if ma == nil {
		ma = &wakatime.MachineEntry{
			Id:    entry.MachineNameId,
			Value: entry.MachineNameId,
		}
	}

	return (&models.Heartbeat{
		User:            user,
		UserID:          user.ID,
		Entity:          entry.Entity,
		Type:            entry.Type,
		Category:        entry.Category,
		Project:         entry.Project,
		Branch:          entry.Branch,
		Language:        entry.Language,
		IsWrite:         entry.IsWrite,
		Editor:          ua.Editor,
		OperatingSystem: ua.Os,
		Machine:         ma.Value,
		Time:            entry.Time,
		Origin:          OriginWakatime,
		OriginId:        entry.Id,
	}).Hashed()
}

func generateDays(from, to time.Time) []time.Time {
	days := make([]time.Time, 0)

	from = utils.StartOfDay(from)
	to = utils.StartOfDay(to.Add(24 * time.Hour))

	for d := from; d.Before(to); d = d.Add(24 * time.Hour) {
		days = append(days, d)
	}

	return days
}