package transcode

import (
	"context"
	"errors"
	"slices"
	"strings"

	"github.com/amwolff/yamp/internal/fsutil"
	"github.com/amwolff/yamp/internal/store"
	"github.com/amwolff/yamp/internal/store/db"
)

// SeedPresets creates the built-in profiles if there are no profiles yet.
func SeedPresets(ctx context.Context, st *store.Store) error {
	return st.Tx(ctx, func(q *db.Queries) error {
		n, err := q.CountProfiles(ctx)
		if err != nil || n > 0 {
			return err
		}
		for _, p := range Presets {
			if _, err := q.CreateProfile(ctx, db.CreateProfileParams{
				Name: p.Name, ParamsJson: p.Params.JSON(), ParamsHash: p.Params.Hash(),
				AutoPolicy: p.Policy, Priority: int64(p.Priority), Builtin: 1,
			}); err != nil {
				return err
			}
		}
		return nil
	})
}

// ProfileInput is a profile as the admin form submits it.
type ProfileInput struct {
	Name     string
	Policy   string
	Priority int64
	Params   Params
}

func (in ProfileInput) validate() error {
	var errs []error
	if n := strings.TrimSpace(in.Name); n == "" || len(n) > 64 {
		errs = append(errs, errors.New("the name must have 1 to 64 characters"))
	}
	if !slices.Contains(Policies, in.Policy) {
		errs = append(errs, errors.New("unknown policy"))
	}
	if in.Priority < -100 || in.Priority > 100 {
		errs = append(errs, errors.New("the priority must be from -100 to 100"))
	}
	if err := in.Params.Validate(); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// CreateProfile adds a profile.
func (q *Queue) CreateProfile(ctx context.Context, in ProfileInput) (int64, error) {
	if err := in.validate(); err != nil {
		return 0, err
	}
	id, err := q.cfg.Store.CreateProfile(ctx, db.CreateProfileParams{
		Name: strings.TrimSpace(in.Name), ParamsJson: in.Params.JSON(), ParamsHash: in.Params.Hash(),
		AutoPolicy: in.Policy, Priority: in.Priority,
	})
	if err == nil {
		q.AutoEnqueue(ctx)
	}
	return id, err
}

// UpdateProfile changes a profile. New parameters mark its renditions stale;
// they keep playing until replaced.
func (q *Queue) UpdateProfile(ctx context.Context, id int64, in ProfileInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	err := q.cfg.Store.UpdateProfile(ctx, db.UpdateProfileParams{
		Name: strings.TrimSpace(in.Name), ParamsJson: in.Params.JSON(), ParamsHash: in.Params.Hash(),
		AutoPolicy: in.Policy, Priority: in.Priority, ID: id,
	})
	if err == nil {
		q.AutoEnqueue(ctx)
	}
	return err
}

// DeleteProfile deletes a profile, its renditions and its jobs.
func (q *Queue) DeleteProfile(ctx context.Context, id int64) error {
	// Stop the running jobs of the profile; the delete cascades to its jobs.
	q.mu.Lock()
	for _, rj := range q.running {
		if rj.profileID == id {
			rj.userCancel = true
			rj.cancel()
		}
	}
	q.mu.Unlock()
	paths, err := q.cfg.Store.ListRenditionPathsForProfile(ctx, id)
	if err != nil {
		return err
	}
	if err := q.cfg.Store.DeleteProfile(ctx, id); err != nil {
		return err
	}
	for _, p := range paths {
		if err := fsutil.RemoveIfExists(p); err != nil {
			q.cfg.Log.Warn("remove rendition failed", "err", err)
		}
	}
	q.publish()
	return nil
}

// RetranscodeStale queues a job for each media item with a stale rendition of
// the profile. It returns the number of queued jobs.
func (q *Queue) RetranscodeStale(ctx context.Context, profileID int64) (int, error) {
	prof, err := q.cfg.Store.GetProfile(ctx, profileID)
	if err != nil {
		return 0, err
	}
	ids, err := q.cfg.Store.ListStaleMediaForProfile(ctx, profileID)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, mid := range ids {
		added, err := q.Enqueue(ctx, mid, profileID, prof.Priority)
		if err != nil {
			return n, err
		}
		if added {
			n++
		}
	}
	return n, nil
}
