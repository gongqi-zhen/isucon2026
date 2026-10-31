package isureal

import "time"

type PostingWindow struct {
	ID             string     `db:"id"`
	Region         string     `db:"region"`
	LocalDate      time.Time  `db:"local_date"`
	StartsAt       time.Time  `db:"starts_at"`
	OnTimeDeadline time.Time  `db:"on_time_deadline"`
	EndedAt        *time.Time `db:"ended_at"`
}
