package platformconfig

import (
	"context"

	"github.com/mutex-solutions/proxcloud/internal/pgdoc"
)

// Repo backs the platformConfiguration collection.
type Repo struct{ col *pgdoc.Store }

func NewRepo(db *pgdoc.DB) *Repo { return &Repo{col: db.C("platformConfiguration")} }

// FindDefault returns the first config doc, or (nil, nil) if none exists.
func (r *Repo) FindDefault(ctx context.Context) (*PlatformConfiguration, error) {
	var c PlatformConfiguration
	found, err := r.col.FindOne(ctx, pgdoc.M{}, &c)
	if err != nil || !found {
		return nil, err
	}
	return &c, nil
}

// AdminDto is the RAW PlatformConfiguration domain as the admin endpoints serialize it: null
// fields omitted, but `regions` and `loginConfiguration` are emitted as non-null empties
// (initialized) — handled in toAdminDto.
type AdminDto struct {
	ID       any    `json:"id"`
	Name     string `json:"name,omitempty"`
	Language string `json:"language,omitempty"`
	// Integration-id selectors the admin config page sets. These were DROPPED before → the FE saved
	// the pick (ReplaceDoc stored it) but the read hid it, so the dropdown showed nothing on reload.
	// Omit when unset.
	MailGatewayID                 string                    `json:"mailGatewayId,omitempty"`
	ContactIntegrationID          string                    `json:"contactIntegrationId,omitempty"`
	SegmentIntegrationID          string                    `json:"segmentIntegrationId,omitempty"`
	Branding                      *Branding                 `json:"branding,omitempty"`
	DefaultConfiguration          bool                      `json:"defaultConfiguration"`
	Regions                       []RegionDisplayConfig     `json:"regions"`
	DateConfiguration             *DateFormat               `json:"dateConfiguration,omitempty"`
	LoginConfiguration            map[string]any            `json:"loginConfiguration"`
	ProjectProvisioningQuota      *ProjectProvisioningQuota `json:"projectProvisioningQuota,omitempty"`
	OrganizationProvisioningQuota *ProvisioningQuota        `json:"organizationProvisioningQuota,omitempty"`
}

// adminConfigDoc reads the raw doc.
type adminConfigDoc struct {
	ID                            string                    `json:"id"`
	Name                          string                    `json:"name"`
	Language                      string                    `json:"language"`
	MailGatewayID                 string                    `json:"mailGatewayId"`
	ContactIntegrationID          string                    `json:"contactIntegrationId"`
	SegmentIntegrationID          string                    `json:"segmentIntegrationId"`
	Branding                      *Branding                 `json:"branding"`
	DefaultConfiguration          bool                      `json:"defaultConfiguration"`
	Regions                       []RegionDisplayConfig     `json:"regions"`
	DateConfiguration             *DateFormat               `json:"dateConfiguration"`
	LoginConfiguration            map[string]any            `json:"loginConfiguration"`
	ProjectProvisioningQuota      *ProjectProvisioningQuota `json:"projectProvisioningQuota"`
	OrganizationProvisioningQuota *ProvisioningQuota        `json:"organizationProvisioningQuota"`
}

func (d adminConfigDoc) toAdminDto() AdminDto {
	regions := d.Regions
	if regions == nil {
		regions = []RegionDisplayConfig{}
	}
	login := d.LoginConfiguration
	if login == nil {
		login = map[string]any{}
	}
	return AdminDto{
		ID: d.ID, Name: d.Name, Language: d.Language,
		MailGatewayID: d.MailGatewayID, ContactIntegrationID: d.ContactIntegrationID, SegmentIntegrationID: d.SegmentIntegrationID,
		Branding:             d.Branding,
		DefaultConfiguration: d.DefaultConfiguration, Regions: regions,
		DateConfiguration: d.DateConfiguration, LoginConfiguration: login,
		ProjectProvisioningQuota:      d.ProjectProvisioningQuota,
		OrganizationProvisioningQuota: d.OrganizationProvisioningQuota,
	}
}

// AllAdminConfigurations returns every admin config doc.
func (r *Repo) AllAdminConfigurations(ctx context.Context) ([]AdminDto, error) {
	var docs []adminConfigDoc
	if err := r.col.Find(ctx, pgdoc.M{}, &docs); err != nil {
		return nil, err
	}
	out := make([]AdminDto, 0, len(docs))
	for _, d := range docs {
		out = append(out, d.toAdminDto())
	}
	return out, nil
}

// ByIDAdminConfiguration fetches the admin config edit-record by id (GET /{id}). Returns
// (nil,nil) when not found → the handler 500s. Same AdminDto shape as list/current so the FE
// binds one model.
func (r *Repo) ByIDAdminConfiguration(ctx context.Context, id string) (*AdminDto, error) {
	var doc adminConfigDoc
	found, err := r.col.FindOne(ctx, pgdoc.M{"_id": id}, &doc)
	if err != nil || !found {
		return nil, err
	}
	dto := doc.toAdminDto()
	return &dto, nil
}

// CurrentAdminConfiguration returns the default config (else first). A fresh install has
// none — then the create branch runs: insert an empty default config and return it, so the
// admin config page always has a real id to save against.
func (r *Repo) CurrentAdminConfiguration(ctx context.Context) (*AdminDto, error) {
	var doc adminConfigDoc
	found, err := r.col.FindOne(ctx, pgdoc.M{"defaultConfiguration": true}, &doc)
	if err != nil {
		return nil, err
	}
	if !found {
		found, err = r.col.FindOne(ctx, pgdoc.M{}, &doc)
		if err != nil {
			return nil, err
		}
	}
	if !found {
		id, err := r.col.InsertOne(ctx, pgdoc.M{"defaultConfiguration": true})
		if err != nil {
			return nil, err
		}
		doc = adminConfigDoc{ID: id, DefaultConfiguration: true}
	}
	dto := doc.toAdminDto()
	return &dto, nil
}
