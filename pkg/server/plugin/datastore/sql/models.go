package sql

import (
	"time"
)

// Using our own model struct to remove DeletedAt. We don't want soft-delete support.
type Model struct {
	ID        uint `gorm:"primary_key"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

type CACert struct {
	Model

	Cert   []byte    `gorm:"not null"`
	Expiry time.Time `gorm:"not null;index"`

	BundleID uint `gorm:"not null;index" sql:"type:integer REFERENCES bundles(id)"`
}

type Bundle struct {
	Model

	TrustDomain string `gorm:"not null;unique_index"`
	CACerts     []CACert
}

type AttestedNodeEntry struct {
	Model

	SpiffeID     string `gorm:"unique_index"`
	DataType     string
	SerialNumber string
	ExpiresAt    time.Time
}

type NodeResolverMapEntry struct {
	Model

	SpiffeID string `gorm:"unique_index:idx_node_resolver_map"`
	Type     string `gorm:"unique_index:idx_node_resolver_map"`
	Value    string `gorm:"unique_index:idx_node_resolver_map"`
}

type RegisteredEntry struct {
	Model

	EntryID   string `gorm:"unique_index"`
	SpiffeID  string
	ParentID  string
	TTL       int32
	Selectors []Selector
	// TODO: Add support to Federated Bundles [https://github.com/spiffe/spire/issues/42]
}

// Keep time simple and easily comparable with UNIX time
type JoinToken struct {
	Model

	Token  string `gorm:"unique_index"`
	Expiry int64
}

type Selector struct {
	Model

	RegisteredEntryID uint   `gorm:"unique_index:idx_selector_entry"`
	Type              string `gorm:"unique_index:idx_selector_entry"`
	Value             string `gorm:"unique_index:idx_selector_entry"`
}

type Migration struct {
	Model

	// Database version
	Version int
}
