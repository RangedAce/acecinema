package main

import "time"

type Site struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	WGEndpoint string    `json:"wg_endpoint"`
	Priority   int       `json:"priority"`
	Weight     int       `json:"weight"`
	CreatedAt  time.Time `json:"created_at"`
}

type Service struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Domains   []string  `json:"domains"`
	CreatedAt time.Time `json:"created_at"`
}

type Backend struct {
	ID          string    `json:"id"`
	ServiceID   string    `json:"service_id"`
	SiteID      string    `json:"site_id"`
	Host        string    `json:"host"`
	Port        int       `json:"port"`
	HealthPath  string    `json:"health_path"`
	Weight      int       `json:"weight"`
	Priority    int       `json:"priority"`
	IsHealthy   bool      `json:"is_healthy"`
	LastChecked time.Time `json:"last_checked"`
}
