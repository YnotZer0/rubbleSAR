package check

import (
	"fmt"
	"strings"
	"time"

	"rubblesar/internal/bus"
	"rubblesar/internal/config"
	"rubblesar/internal/sarpb"
)

// ResetServices archives casualty-analyst's casualties (as <stamp>) and
// clears mission-planner's tasks: the start of a new run, as the acceptance
// runs do it. rescue-dispatch is reset separately (POST /v1/reset).
func ResetServices(cfg *config.ZBook, stamp string) error { return resetServices(cfg, stamp) }

// ServicesUp reports whether casualty-analyst and mission-planner answer.
func ServicesUp(cfg *config.ZBook) error { return servicesUp(cfg) }

// RestartMissions restarts the drones' missions (clearing their tracks and
// queues) and waits for each to confirm.
func RestartMissions(b bus.Bus, incident string, uavs []string, timeout time.Duration) error {
	ctl, err := newController(b, incident)
	if err != nil {
		return err
	}
	defer ctl.close()
	var bad []string
	for _, id := range uavs {
		if _, _, err := ctl.send(id, func(c *sarpb.Control) {
			c.Cmd = &sarpb.Control_RestartMission{RestartMission: &sarpb.RestartMission{SimTimeS: -1, ClearTracks: true, ClearQueue: true}}
		}, timeout); err != nil {
			bad = append(bad, fmt.Sprintf("%s: %v", id, err))
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("restarting the missions: %s", strings.Join(bad, "; "))
	}
	return nil
}

// SetLinkBudget changes a drone's radio budget (bits per second, 0 = unlimited) and waits for it to confirm.
func SetLinkBudget(b bus.Bus, incident, uav string, bps uint32, timeout time.Duration) error {
	ctl, err := newController(b, incident)
	if err != nil {
		return err
	}
	defer ctl.close()
	_, _, err = ctl.send(uav, func(c *sarpb.Control) {
		c.Cmd = &sarpb.Control_SetLinkBudget{SetLinkBudget: &sarpb.SetLinkBudget{Bps: bps}}
	}, timeout)
	return err
}

// SetLinkOutage silences a drone's radio for seconds (a simulated blackout); the drone confirms before it goes quiet.
func SetLinkOutage(b bus.Bus, incident, uav string, seconds uint32, timeout time.Duration) error {
	ctl, err := newController(b, incident)
	if err != nil {
		return err
	}
	defer ctl.close()
	_, _, err = ctl.send(uav, func(c *sarpb.Control) {
		c.Cmd = &sarpb.Control_SetLinkOutage{SetLinkOutage: &sarpb.SetLinkOutage{Seconds: seconds}}
	}, timeout)
	return err
}
