package router

import "time"

type modelActivity struct {
	readyAt     time.Time
	lastServeAt time.Time
	lastDoneAt  time.Time
	active      int
}

func (a modelActivity) idleSince() time.Time {
	if !a.lastDoneAt.IsZero() {
		return a.lastDoneAt
	}
	if !a.readyAt.IsZero() {
		return a.readyAt
	}
	return a.lastServeAt
}

func (a modelActivity) activityAt() time.Time {
	if !a.lastDoneAt.IsZero() {
		return a.lastDoneAt
	}
	if !a.lastServeAt.IsZero() {
		return a.lastServeAt
	}
	return a.readyAt
}
