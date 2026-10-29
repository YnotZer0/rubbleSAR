package live

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"rubblesar/internal/sarpb"
)

// Services are the ZBook services' local status APIs ([analyst], [planner], [dispatch] listen).
type Services struct {
	Analyst, Planner, Dispatch string
	Client                     *http.Client
}

func (s Services) client() *http.Client {
	if s.Client != nil {
		return s.Client
	}
	return &http.Client{Timeout: 3 * time.Second}
}

func local(addr string) string {
	if strings.HasPrefix(addr, "0.0.0.0:") {
		return "127.0.0.1:" + strings.TrimPrefix(addr, "0.0.0.0:")
	}
	return addr
}

func (s Services) get(addr, path string) ([]byte, error) {
	if addr == "" {
		return nil, fmt.Errorf("no address")
	}
	resp, err := s.client().Get("http://" + local(addr) + path)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s%s: %s", addr, path, resp.Status)
	}
	return b, nil
}

var pj = protojson.UnmarshalOptions{DiscardUnknown: true}

// Seed fills the hub from the services' APIs (sar-web started in the middle of a run:
// the picture is complete at once instead of after the next lists on the bus).
func (s Services) Seed(h *Hub) []error {
	var errs []error
	now := time.Now()
	if b, err := s.get(s.Dispatch, "/v1/state"); err == nil {
		var st sarpb.DispatchState
		if err := pj.Unmarshal(b, &st); err == nil {
			h.SetDispatch(FromDispatch(&st), now)
		} else {
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, err)
	}
	if b, err := s.get(s.Analyst, "/v1/casualties"); err == nil {
		var cl sarpb.CasualtyList
		if err := pj.Unmarshal(b, &cl); err == nil {
			list := make([]*Casualty, 0, len(cl.Casualties))
			for _, c := range cl.Casualties {
				list = append(list, FromCasualty(c))
			}
			h.SetCasualties(list, now)
		} else {
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, err)
	}
	if b, err := s.get(s.Planner, "/v1/tasks"); err == nil {
		var tl sarpb.TaskList
		if err := pj.Unmarshal(b, &tl); err == nil {
			list := make([]*Task, 0, len(tl.Tasks))
			for _, t := range tl.Tasks {
				list = append(list, FromTask(t))
			}
			h.SetTasks(list, now)
		} else {
			errs = append(errs, err)
		}
	} else {
		errs = append(errs, err)
	}
	return errs
}

// Chats reads rescue-dispatch's chat (GET /v1/chats).
func (s Services) Chats() ([]*Chat, error) {
	b, err := s.get(s.Dispatch, "/v1/chats")
	if err != nil {
		return nil, err
	}
	var lines []struct {
		At   time.Time `json:"at"`
		From string    `json:"from"`
		To   string    `json:"to"`
		Text string    `json:"text"`
	}
	if err := json.Unmarshal(b, &lines); err != nil {
		return nil, err
	}
	out := make([]*Chat, 0, len(lines))
	for _, l := range lines {
		out = append(out, &Chat{At: l.At.UnixMilli(), From: l.From, To: l.To, Text: l.Text})
	}
	return out, nil
}

// CasualtyIDs, TaskStates and AssignmentStates read the services directly (the phase 7 checks compare them with the picture).
func (s Services) CasualtyIDs() (map[string]string, error) {
	b, err := s.get(s.Analyst, "/v1/casualties")
	if err != nil {
		return nil, err
	}
	var cl sarpb.CasualtyList
	if err := pj.Unmarshal(b, &cl); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, c := range cl.Casualties {
		out[c.CasualtyId] = FromCasualty(c).Triage
	}
	return out, nil
}

// AssignmentStates is rescue-dispatch's assignments: id -> state.
func (s Services) AssignmentStates() (map[string]string, error) {
	b, err := s.get(s.Dispatch, "/v1/state")
	if err != nil {
		return nil, err
	}
	var st sarpb.DispatchState
	if err := pj.Unmarshal(b, &st); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, a := range st.Assignments {
		out[a.AssignmentId] = FromAssignment(a).State
	}
	return out, nil
}

// TaskStates is mission-planner's tasks: id -> state.
func (s Services) TaskStates() (map[string]string, error) {
	b, err := s.get(s.Planner, "/v1/tasks")
	if err != nil {
		return nil, err
	}
	var tl sarpb.TaskList
	if err := pj.Unmarshal(b, &tl); err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, t := range tl.Tasks {
		out[t.TaskId] = FromTask(t).State
	}
	return out, nil
}
