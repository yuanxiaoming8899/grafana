package folderimpl

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"golang.org/x/exp/slices"

	"github.com/grafana/grafana/pkg/bus"
	"github.com/grafana/grafana/pkg/events"
	"github.com/grafana/grafana/pkg/infra/db"
	"github.com/grafana/grafana/pkg/infra/log"
	"github.com/grafana/grafana/pkg/services/accesscontrol"
	"github.com/grafana/grafana/pkg/services/auth/identity"
	"github.com/grafana/grafana/pkg/services/dashboards"
	"github.com/grafana/grafana/pkg/services/featuremgmt"
	"github.com/grafana/grafana/pkg/services/folder"
	"github.com/grafana/grafana/pkg/services/guardian"
	"github.com/grafana/grafana/pkg/services/sqlstore"
	"github.com/grafana/grafana/pkg/services/sqlstore/migrator"
	"github.com/grafana/grafana/pkg/services/store/entity"
	"github.com/grafana/grafana/pkg/setting"
	"github.com/grafana/grafana/pkg/util"
)

type Service struct {
	store                store
	db                   db.DB
	log                  log.Logger
	cfg                  *setting.Cfg
	dashboardStore       dashboards.Store
	dashboardFolderStore folder.FolderStore
	features             featuremgmt.FeatureToggles
	accessControl        accesscontrol.AccessControl

	// bus is currently used to publish event in case of title change
	bus bus.Bus

	mutex    sync.RWMutex
	registry map[string]folder.RegistryService
}

func ProvideService(
	ac accesscontrol.AccessControl,
	bus bus.Bus,
	cfg *setting.Cfg,
	dashboardStore dashboards.Store,
	folderStore folder.FolderStore,
	db db.DB, // DB for the (new) nested folder store
	features featuremgmt.FeatureToggles,
) folder.Service {
	store := ProvideStore(db, cfg, features)
	srv := &Service{
		cfg:                  cfg,
		log:                  log.New("folder-service"),
		dashboardStore:       dashboardStore,
		dashboardFolderStore: folderStore,
		store:                store,
		features:             features,
		accessControl:        ac,
		bus:                  bus,
		db:                   db,
		registry:             make(map[string]folder.RegistryService),
	}
	srv.DBMigration(db)

	ac.RegisterScopeAttributeResolver(dashboards.NewFolderNameScopeResolver(folderStore, srv))
	ac.RegisterScopeAttributeResolver(dashboards.NewFolderIDScopeResolver(folderStore, srv))
	ac.RegisterScopeAttributeResolver(dashboards.NewFolderUIDScopeResolver(srv))
	return srv
}

func (s *Service) DBMigration(db db.DB) {
	ctx := context.Background()
	err := db.WithDbSession(ctx, func(sess *sqlstore.DBSession) error {
		var err error
		if db.GetDialect().DriverName() == migrator.SQLite {
			_, err = sess.Exec(`
				INSERT INTO folder (uid, org_id, title, created, updated)
				SELECT uid, org_id, title, created, updated FROM dashboard WHERE is_folder = 1
				ON CONFLICT DO UPDATE SET title=excluded.title, updated=excluded.updated
			`)
		} else if db.GetDialect().DriverName() == migrator.Postgres {
			_, err = sess.Exec(`
				INSERT INTO folder (uid, org_id, title, created, updated)
				SELECT uid, org_id, title, created, updated FROM dashboard WHERE is_folder = true
				ON CONFLICT(uid, org_id) DO UPDATE SET title=excluded.title, updated=excluded.updated
			`)
		} else {
			_, err = sess.Exec(`
				INSERT INTO folder (uid, org_id, title, created, updated)
				SELECT * FROM (SELECT uid, org_id, title, created, updated FROM dashboard WHERE is_folder = 1) AS derived
				ON DUPLICATE KEY UPDATE title=derived.title, updated=derived.updated
			`)
		}
		if err != nil {
			return err
		}
		_, err = sess.Exec(`
			DELETE FROM folder WHERE NOT EXISTS
				(SELECT 1 FROM dashboard WHERE dashboard.uid = folder.uid AND dashboard.org_id = folder.org_id AND dashboard.is_folder = true)
		`)
		return err
	})
	if err != nil {
		s.log.Error("DB migration on folder service start failed.", "err", err)
	}
}

func (s *Service) Get(ctx context.Context, cmd *folder.GetFolderQuery) (*folder.Folder, error) {
	if cmd.SignedInUser == nil {
		return nil, folder.ErrBadRequest.Errorf("missing signed in user")
	}

	var dashFolder *folder.Folder
	var err error
	switch {
	case cmd.UID != nil && *cmd.UID != "":
		dashFolder, err = s.getFolderByUID(ctx, cmd.OrgID, *cmd.UID)
		if err != nil {
			return nil, err
		}
	case cmd.ID != nil:
		dashFolder, err = s.getFolderByID(ctx, *cmd.ID, cmd.OrgID)
		if err != nil {
			return nil, err
		}
	case cmd.Title != nil:
		dashFolder, err = s.getFolderByTitle(ctx, cmd.OrgID, *cmd.Title)
		if err != nil {
			return nil, err
		}
	default:
		return nil, folder.ErrBadRequest.Errorf("either on of UID, ID, Title fields must be present")
	}

	if dashFolder.IsGeneral() {
		return dashFolder, nil
	}

	// do not get guardian by the folder ID because it differs from the nested folder ID
	// and the legacy folder ID has been associated with the permissions:
	// use the folde UID instead that is the same for both
	g, err := guardian.NewByFolder(ctx, dashFolder, dashFolder.OrgID, cmd.SignedInUser)
	if err != nil {
		return nil, err
	}

	if canView, err := g.CanView(); err != nil || !canView {
		if err != nil {
			return nil, toFolderError(err)
		}
		return nil, dashboards.ErrFolderAccessDenied
	}

	if !s.features.IsEnabled(featuremgmt.FlagNestedFolders) {
		return dashFolder, nil
	}

	if cmd.ID != nil {
		cmd.ID = nil
		cmd.UID = &dashFolder.UID
	}

	f, err := s.store.Get(ctx, *cmd)
	if err != nil {
		return nil, err
	}

	// always expose the dashboard store sequential ID
	f.ID = dashFolder.ID
	f.Version = dashFolder.Version

	return f, err
}

func (s *Service) GetChildren(ctx context.Context, cmd *folder.GetChildrenQuery) ([]*folder.Folder, error) {
	if cmd.SignedInUser == nil {
		return nil, folder.ErrBadRequest.Errorf("missing signed in user")
	}

	if cmd.UID != "" {
		g, err := guardian.NewByUID(ctx, cmd.UID, cmd.OrgID, cmd.SignedInUser)
		if err != nil {
			return nil, err
		}

		canView, err := g.CanView()
		if err != nil {
			return nil, err
		}

		if !canView {
			return nil, dashboards.ErrFolderAccessDenied
		}
	}

	children, err := s.store.GetChildren(ctx, *cmd)
	if err != nil {
		return nil, err
	}

	childrenUIDs := make([]string, 0, len(children))
	for _, f := range children {
		childrenUIDs = append(childrenUIDs, f.UID)
	}

	dashFolders, err := s.dashboardFolderStore.GetFolders(ctx, cmd.OrgID, childrenUIDs)
	if err != nil {
		return nil, folder.ErrInternal.Errorf("failed to fetch subfolders from dashboard store: %w", err)
	}

	filtered := make([]*folder.Folder, 0, len(children))
	for _, f := range children {
		// fetch folder from dashboard store
		dashFolder, ok := dashFolders[f.UID]
		if !ok {
			s.log.Error("failed to fetch folder by UID from dashboard store", "uid", f.UID)
			continue
		}

		// always expose the dashboard store sequential ID
		f.ID = dashFolder.ID

		if cmd.UID != "" {
			// parent access has been checked already
			// the subfolder must be accessible as well (due to inheritance)
			filtered = append(filtered, f)
			continue
		}

		g, err := guardian.NewByFolder(ctx, dashFolder, dashFolder.OrgID, cmd.SignedInUser)
		if err != nil {
			return nil, err
		}
		canView, err := g.CanView()
		if err != nil {
			return nil, err
		}
		if canView {
			filtered = append(filtered, f)
		}
	}

	return filtered, nil
}

// GetSharedWithMe returns folders available to user, which cannot be accessed from the root folders
func (s *Service) GetSharedWithMe(ctx context.Context, cmd *folder.GetChildrenQuery) ([]*folder.Folder, error) {
	availableNonRootFolders, err := s.getAvailableNonRootFolders(ctx, cmd.OrgID, cmd.SignedInUser)
	if err != nil {
		return nil, folder.ErrInternal.Errorf("failed to fetch subfolders to which the user has explicit access: %w", err)
	}
	rootFolders, err := s.GetChildren(ctx, &folder.GetChildrenQuery{UID: "", OrgID: cmd.OrgID, SignedInUser: cmd.SignedInUser})
	if err != nil {
		return nil, folder.ErrInternal.Errorf("failed to fetch root folders to which the user has access: %w", err)
	}
	availableNonRootFolders = s.deduplicateAvailableFolders(ctx, availableNonRootFolders, rootFolders)
	return availableNonRootFolders, nil
}

func (s *Service) getAvailableNonRootFolders(ctx context.Context, orgID int64, user identity.Requester) ([]*folder.Folder, error) {
	permissions := user.GetPermissions()
	folderPermissions := permissions["folders:read"]
	folderPermissions = append(folderPermissions, permissions["dashboards:read"]...)
	nonRootFolders := make([]*folder.Folder, 0)
	folderUids := make([]string, 0)
	for _, p := range folderPermissions {
		if folderUid, found := strings.CutPrefix(p, "folders:uid:"); found {
			if !slices.Contains(folderUids, folderUid) {
				folderUids = append(folderUids, folderUid)
			}
		}
	}

	if len(folderUids) == 0 {
		return nonRootFolders, nil
	}

	dashFolders, err := s.store.GetFolders(ctx, orgID, folderUids)
	if err != nil {
		return nil, folder.ErrInternal.Errorf("failed to fetch subfolders: %w", err)
	}

	for _, f := range dashFolders {
		if f.ParentUID != "" {
			nonRootFolders = append(nonRootFolders, f)
		}
	}

	return nonRootFolders, nil
}

func (s *Service) deduplicateAvailableFolders(ctx context.Context, folders []*folder.Folder, rootFolders []*folder.Folder) []*folder.Folder {
	allFolders := append(folders, rootFolders...)
	foldersDedup := make([]*folder.Folder, 0)
	for _, f := range folders {
		isSubfolder := slices.ContainsFunc(allFolders, func(folder *folder.Folder) bool {
			return f.ParentUID == folder.UID
		})

		if !isSubfolder {
			parents, err := s.GetParents(ctx, folder.GetParentsQuery{UID: f.UID, OrgID: f.OrgID})
			if err != nil {
				s.log.Error("failed to fetch folder parents", "uid", f.UID, "error", err)
				continue
			}

			for _, parent := range parents {
				contains := slices.ContainsFunc(allFolders, func(f *folder.Folder) bool {
					return f.UID == parent.UID
				})
				if contains {
					isSubfolder = true
					break
				}
			}
		}

		if !isSubfolder {
			foldersDedup = append(foldersDedup, f)
		}
	}
	return foldersDedup
}

func (s *Service) GetParents(ctx context.Context, q folder.GetParentsQuery) ([]*folder.Folder, error) {
	if !s.features.IsEnabled(featuremgmt.FlagNestedFolders) {
		return nil, nil
	}
	return s.store.GetParents(ctx, q)
}

func (s *Service) getFolderByID(ctx context.Context, id int64, orgID int64) (*folder.Folder, error) {
	if id == 0 {
		return &folder.GeneralFolder, nil
	}

	return s.dashboardFolderStore.GetFolderByID(ctx, orgID, id)
}

func (s *Service) getFolderByUID(ctx context.Context, orgID int64, uid string) (*folder.Folder, error) {
	return s.dashboardFolderStore.GetFolderByUID(ctx, orgID, uid)
}

func (s *Service) getFolderByTitle(ctx context.Context, orgID int64, title string) (*folder.Folder, error) {
	return s.dashboardFolderStore.GetFolderByTitle(ctx, orgID, title)
}

func (s *Service) Create(ctx context.Context, cmd *folder.CreateFolderCommand) (*folder.Folder, error) {
	logger := s.log.FromContext(ctx)

	if cmd.SignedInUser == nil || cmd.SignedInUser.IsNil() {
		return nil, folder.ErrBadRequest.Errorf("missing signed in user")
	}

	dashFolder := dashboards.NewDashboardFolder(cmd.Title)
	dashFolder.OrgID = cmd.OrgID

	if s.features.IsEnabled(featuremgmt.FlagNestedFolders) && cmd.ParentUID != "" {
		// Check that the user is allowed to create a subfolder in this folder
		evaluator := accesscontrol.EvalPermission(dashboards.ActionFoldersWrite, dashboards.ScopeFoldersProvider.GetResourceScopeUID(cmd.ParentUID))
		hasAccess, evalErr := s.accessControl.Evaluate(ctx, cmd.SignedInUser, evaluator)
		if evalErr != nil {
			return nil, evalErr
		}
		if !hasAccess {
			return nil, dashboards.ErrFolderAccessDenied
		}
		dashFolder.FolderUID = cmd.ParentUID
	}

	trimmedUID := strings.TrimSpace(cmd.UID)
	if trimmedUID == accesscontrol.GeneralFolderUID {
		return nil, dashboards.ErrFolderInvalidUID
	}

	dashFolder.SetUID(trimmedUID)

	user := cmd.SignedInUser

	userID := int64(0)
	var err error
	namespaceID, userIDstr := user.GetNamespacedID()
	if namespaceID != identity.NamespaceUser && namespaceID != identity.NamespaceServiceAccount {
		s.log.Debug("User does not belong to a user or service account namespace, using 0 as user ID", "namespaceID", namespaceID, "userID", userIDstr)
	} else {
		userID, err = identity.IntIdentifier(namespaceID, userIDstr)
		if err != nil {
			s.log.Debug("failed to parse user ID", "namespaceID", namespaceID, "userID", userIDstr, "error", err)
		}
	}

	if userID == 0 {
		userID = -1
	}
	dashFolder.CreatedBy = userID
	dashFolder.UpdatedBy = userID
	dashFolder.UpdateSlug()

	dto := &dashboards.SaveDashboardDTO{
		Dashboard: dashFolder,
		OrgID:     cmd.OrgID,
		User:      user,
	}

	saveDashboardCmd, err := s.buildSaveDashboardCommand(ctx, dto)
	if err != nil {
		return nil, toFolderError(err)
	}

	dash, err := s.dashboardStore.SaveDashboard(ctx, *saveDashboardCmd)
	if err != nil {
		return nil, toFolderError(err)
	}

	var createdFolder *folder.Folder
	createdFolder, err = s.dashboardFolderStore.GetFolderByID(ctx, cmd.OrgID, dash.ID)
	if err != nil {
		return nil, err
	}

	var nestedFolder *folder.Folder
	cmd = &folder.CreateFolderCommand{
		// TODO: Today, if a UID isn't specified, the dashboard store
		// generates a new UID. The new folder store will need to do this as
		// well, but for now we take the UID from the newly created folder.
		UID:         dash.UID,
		OrgID:       cmd.OrgID,
		Title:       cmd.Title,
		Description: cmd.Description,
		ParentUID:   cmd.ParentUID,
	}
	nestedFolder, err = s.nestedFolderCreate(ctx, cmd)
	if err != nil {
		// We'll log the error and also roll back the previously-created
		// (legacy) folder.
		logger.Error("error saving folder to nested folder store", "error", err)
		// do not shallow create error if the legacy folder delete fails
		if deleteErr := s.dashboardStore.DeleteDashboard(ctx, &dashboards.DeleteDashboardCommand{
			ID:    createdFolder.ID,
			OrgID: createdFolder.OrgID,
		}); deleteErr != nil {
			logger.Error("error deleting folder after failed save to nested folder store", "error", err)
		}
		return dashboards.FromDashboard(dash), err
	}

	f := dashboards.FromDashboard(dash)
	if nestedFolder != nil && nestedFolder.ParentUID != "" {
		f.ParentUID = nestedFolder.ParentUID
	}
	return f, nil
}

func (s *Service) Update(ctx context.Context, cmd *folder.UpdateFolderCommand) (*folder.Folder, error) {
	logger := s.log.FromContext(ctx)

	if cmd.SignedInUser == nil {
		return nil, folder.ErrBadRequest.Errorf("missing signed in user")
	}
	user := cmd.SignedInUser

	dashFolder, err := s.legacyUpdate(ctx, cmd)
	if err != nil {
		return nil, err
	}

	foldr, err := s.store.Update(ctx, folder.UpdateFolderCommand{
		UID:            cmd.UID,
		OrgID:          cmd.OrgID,
		NewTitle:       cmd.NewTitle,
		NewDescription: cmd.NewDescription,
		SignedInUser:   user,
	})
	if err != nil {
		if errors.Is(err, folder.ErrFolderNotFound) {
			logger.Warn("attempt to update folder that does not exist in the folders table", "folderUID", cmd.UID)
			return dashFolder, nil
		}

		return nil, err
	}

	// always expose the dashboard store sequential ID
	foldr.ID = dashFolder.ID
	foldr.Version = dashFolder.Version

	return foldr, nil
}

func (s *Service) legacyUpdate(ctx context.Context, cmd *folder.UpdateFolderCommand) (*folder.Folder, error) {
	logger := s.log.FromContext(ctx)

	query := dashboards.GetDashboardQuery{OrgID: cmd.OrgID, UID: cmd.UID}
	queryResult, err := s.dashboardStore.GetDashboard(ctx, &query)
	if err != nil {
		return nil, toFolderError(err)
	}

	dashFolder := queryResult
	if cmd.NewParentUID != nil {
		dashFolder.FolderUID = *cmd.NewParentUID
	}

	currentTitle := dashFolder.Title

	if !dashFolder.IsFolder {
		return nil, dashboards.ErrFolderNotFound
	}

	if cmd.SignedInUser == nil {
		return nil, folder.ErrBadRequest.Errorf("missing signed in user")
	}

	var userID int64
	namespace, id := cmd.SignedInUser.GetNamespacedID()
	if namespace == identity.NamespaceUser || namespace == identity.NamespaceServiceAccount {
		userID, err = identity.IntIdentifier(namespace, id)
		if err != nil {
			logger.Error("failed to parse user ID", "namespace", namespace, "userID", id, "error", err)
		}
	}

	prepareForUpdate(dashFolder, cmd.OrgID, userID, cmd)

	dto := &dashboards.SaveDashboardDTO{
		Dashboard: dashFolder,
		OrgID:     cmd.OrgID,
		User:      cmd.SignedInUser,
		Overwrite: cmd.Overwrite,
	}

	saveDashboardCmd, err := s.buildSaveDashboardCommand(ctx, dto)
	if err != nil {
		return nil, toFolderError(err)
	}

	dash, err := s.dashboardStore.SaveDashboard(ctx, *saveDashboardCmd)
	if err != nil {
		return nil, toFolderError(err)
	}

	var foldr *folder.Folder
	foldr, err = s.dashboardFolderStore.GetFolderByID(ctx, cmd.OrgID, dash.ID)
	if err != nil {
		return nil, err
	}

	if currentTitle != foldr.Title {
		if err := s.bus.Publish(ctx, &events.FolderTitleUpdated{
			Timestamp: foldr.Updated,
			Title:     foldr.Title,
			ID:        dash.ID,
			UID:       dash.UID,
			OrgID:     cmd.OrgID,
		}); err != nil {
			logger.Error("failed to publish FolderTitleUpdated event", "folder", foldr.Title, "user", id, "namespace", namespace, "error", err)
		}
	}
	return foldr, nil
}

// prepareForUpdate updates an existing dashboard model from command into model for folder update
func prepareForUpdate(dashFolder *dashboards.Dashboard, orgId int64, userId int64, cmd *folder.UpdateFolderCommand) {
	dashFolder.OrgID = orgId

	title := dashFolder.Title
	if cmd.NewTitle != nil && *cmd.NewTitle != "" {
		title = *cmd.NewTitle
	}
	dashFolder.Title = strings.TrimSpace(title)
	dashFolder.Data.Set("title", dashFolder.Title)

	dashFolder.SetVersion(cmd.Version)
	dashFolder.IsFolder = true

	if userId == 0 {
		userId = -1
	}

	dashFolder.UpdatedBy = userId
	dashFolder.UpdateSlug()
}

func (s *Service) Delete(ctx context.Context, cmd *folder.DeleteFolderCommand) error {
	logger := s.log.FromContext(ctx)
	if cmd.SignedInUser == nil {
		return folder.ErrBadRequest.Errorf("missing signed in user")
	}
	if cmd.UID == "" {
		return folder.ErrBadRequest.Errorf("missing UID")
	}
	if cmd.OrgID < 1 {
		return folder.ErrBadRequest.Errorf("invalid orgID")
	}

	guard, err := guardian.NewByUID(ctx, cmd.UID, cmd.OrgID, cmd.SignedInUser)
	if err != nil {
		return err
	}

	if canSave, err := guard.CanDelete(); err != nil || !canSave {
		if err != nil {
			return toFolderError(err)
		}
		return dashboards.ErrFolderAccessDenied
	}

	result := []string{cmd.UID}
	err = s.db.InTransaction(ctx, func(ctx context.Context) error {
		subfolders, err := s.nestedFolderDelete(ctx, cmd)

		if err != nil {
			logger.Error("the delete folder on folder table failed with err: ", "error", err)
			return err
		}
		result = append(result, subfolders...)

		dashFolders, err := s.dashboardFolderStore.GetFolders(ctx, cmd.OrgID, result)
		if err != nil {
			return folder.ErrInternal.Errorf("failed to fetch subfolders from dashboard store: %w", err)
		}

		for _, folder := range result {
			dashFolder, ok := dashFolders[folder]
			if !ok {
				return err
			}

			if cmd.ForceDeleteRules {
				if err := s.deleteChildrenInFolder(ctx, dashFolder.OrgID, dashFolder.UID, cmd.SignedInUser); err != nil {
					return err
				}
			}

			err = s.legacyDelete(ctx, cmd, dashFolder)
			if err != nil {
				return err
			}
		}
		return nil
	})

	return err
}

func (s *Service) deleteChildrenInFolder(ctx context.Context, orgID int64, folderUID string, user identity.Requester) error {
	for _, v := range s.registry {
		if err := v.DeleteInFolder(ctx, orgID, folderUID, user); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) legacyDelete(ctx context.Context, cmd *folder.DeleteFolderCommand, dashFolder *folder.Folder) error {
	deleteCmd := dashboards.DeleteDashboardCommand{OrgID: cmd.OrgID, ID: dashFolder.ID, ForceDeleteFolderRules: cmd.ForceDeleteRules}

	if err := s.dashboardStore.DeleteDashboard(ctx, &deleteCmd); err != nil {
		return toFolderError(err)
	}
	return nil
}

func (s *Service) Move(ctx context.Context, cmd *folder.MoveFolderCommand) (*folder.Folder, error) {
	if cmd.SignedInUser == nil {
		return nil, folder.ErrBadRequest.Errorf("missing signed in user")
	}

	// Check that the user is allowed to move the folder to the destination folder
	var evaluator accesscontrol.Evaluator
	if cmd.NewParentUID != "" {
		evaluator = accesscontrol.EvalPermission(dashboards.ActionFoldersWrite, dashboards.ScopeFoldersProvider.GetResourceScopeUID(cmd.NewParentUID))
	} else {
		// Evaluate folder creation permission when moving folder to the root level
		evaluator = accesscontrol.EvalPermission(dashboards.ActionFoldersCreate)
	}
	hasAccess, evalErr := s.accessControl.Evaluate(ctx, cmd.SignedInUser, evaluator)
	if evalErr != nil {
		return nil, evalErr
	}
	if !hasAccess {
		return nil, dashboards.ErrFolderAccessDenied
	}

	// here we get the folder, we need to get the height of current folder
	// and the depth of the new parent folder, the sum can't bypass 8
	folderHeight, err := s.store.GetHeight(ctx, cmd.UID, cmd.OrgID, &cmd.NewParentUID)
	if err != nil {
		return nil, err
	}
	parents, err := s.store.GetParents(ctx, folder.GetParentsQuery{UID: cmd.NewParentUID, OrgID: cmd.OrgID})
	if err != nil {
		return nil, err
	}

	// current folder height + current folder + parent folder + parent folder depth should be less than or equal 8
	if folderHeight+len(parents)+2 > folder.MaxNestedFolderDepth {
		return nil, folder.ErrMaximumDepthReached.Errorf("failed to move folder")
	}

	// if the current folder is already a parent of newparent, we should return error
	for _, parent := range parents {
		if parent.UID == cmd.UID {
			return nil, folder.ErrCircularReference.Errorf("failed to move folder")
		}
	}

	newParentUID := ""
	if cmd.NewParentUID != "" {
		newParentUID = cmd.NewParentUID
	}

	var f *folder.Folder
	if err := s.db.InTransaction(ctx, func(ctx context.Context) error {
		if f, err = s.store.Update(ctx, folder.UpdateFolderCommand{
			UID:          cmd.UID,
			OrgID:        cmd.OrgID,
			NewParentUID: &newParentUID,
			SignedInUser: cmd.SignedInUser,
		}); err != nil {
			return folder.ErrInternal.Errorf("failed to move folder: %w", err)
		}

		if _, err := s.legacyUpdate(ctx, &folder.UpdateFolderCommand{
			UID:          cmd.UID,
			OrgID:        cmd.OrgID,
			NewParentUID: &newParentUID,
			SignedInUser: cmd.SignedInUser,
			// bypass optimistic locking used for dashboards
			Overwrite: true,
		}); err != nil {
			return folder.ErrInternal.Errorf("failed to move legacy folder: %w", err)
		}

		return nil
	}); err != nil {
		return nil, err
	}
	return f, nil
}

// nestedFolderDelete inspects the folder referenced by the cmd argument, deletes all the entries for
// its descendant folders (folders which are nested within it either directly or indirectly) from
// the folder store and returns the UIDs for all its descendants.
func (s *Service) nestedFolderDelete(ctx context.Context, cmd *folder.DeleteFolderCommand) ([]string, error) {
	logger := s.log.FromContext(ctx)
	result := []string{}
	if cmd.SignedInUser == nil {
		return result, folder.ErrBadRequest.Errorf("missing signed in user")
	}

	_, err := s.Get(ctx, &folder.GetFolderQuery{
		UID:          &cmd.UID,
		OrgID:        cmd.OrgID,
		SignedInUser: cmd.SignedInUser,
	})
	if err != nil {
		return result, err
	}

	folders, err := s.store.GetChildren(ctx, folder.GetChildrenQuery{UID: cmd.UID, OrgID: cmd.OrgID})
	if err != nil {
		return result, err
	}
	for _, f := range folders {
		result = append(result, f.UID)
		logger.Info("deleting subfolder", "org_id", f.OrgID, "uid", f.UID)
		subfolders, err := s.nestedFolderDelete(ctx, &folder.DeleteFolderCommand{UID: f.UID, OrgID: f.OrgID, ForceDeleteRules: cmd.ForceDeleteRules, SignedInUser: cmd.SignedInUser})
		if err != nil {
			logger.Error("failed deleting subfolder", "org_id", f.OrgID, "uid", f.UID, "error", err)
			return result, err
		}
		result = append(result, subfolders...)
	}

	logger.Info("deleting folder and its contents", "org_id", cmd.OrgID, "uid", cmd.UID)
	err = s.store.Delete(ctx, cmd.UID, cmd.OrgID)
	if err != nil {
		logger.Info("failed deleting folder", "org_id", cmd.OrgID, "uid", cmd.UID, "err", err)
		return result, err
	}
	return result, nil
}

func (s *Service) GetDescendantCounts(ctx context.Context, cmd *folder.GetDescendantCountsQuery) (folder.DescendantCounts, error) {
	logger := s.log.FromContext(ctx)
	if cmd.SignedInUser == nil {
		return nil, folder.ErrBadRequest.Errorf("missing signed-in user")
	}
	if *cmd.UID == "" {
		return nil, folder.ErrBadRequest.Errorf("missing UID")
	}
	if cmd.OrgID < 1 {
		return nil, folder.ErrBadRequest.Errorf("invalid orgID")
	}

	result := []string{*cmd.UID}
	countsMap := make(folder.DescendantCounts, len(s.registry)+1)
	if s.features.IsEnabled(featuremgmt.FlagNestedFolders) {
		subfolders, err := s.getNestedFolders(ctx, cmd.OrgID, *cmd.UID)
		if err != nil {
			logger.Error("failed to get subfolders", "error", err)
			return nil, err
		}
		result = append(result, subfolders...)
		countsMap[entity.StandardKindFolder] = int64(len(subfolders))
	}

	for _, v := range s.registry {
		for _, folder := range result {
			c, err := v.CountInFolder(ctx, cmd.OrgID, folder, cmd.SignedInUser)
			if err != nil {
				logger.Error("failed to count folder descendants", "error", err)
				return nil, err
			}
			countsMap[v.Kind()] += c
		}
	}
	return countsMap, nil
}

func (s *Service) getNestedFolders(ctx context.Context, orgID int64, uid string) ([]string, error) {
	result := []string{}
	folders, err := s.store.GetChildren(ctx, folder.GetChildrenQuery{UID: uid, OrgID: orgID})
	if err != nil {
		return nil, err
	}

	for _, f := range folders {
		result = append(result, f.UID)
		subfolders, err := s.getNestedFolders(ctx, f.OrgID, f.UID)
		if err != nil {
			return nil, err
		}
		result = append(result, subfolders...)
	}
	return result, nil
}

// buildSaveDashboardCommand is a simplified version on DashboardServiceImpl.buildSaveDashboardCommand
// keeping only the meaningful functionality for folders
func (s *Service) buildSaveDashboardCommand(ctx context.Context, dto *dashboards.SaveDashboardDTO) (*dashboards.SaveDashboardCommand, error) {
	dash := dto.Dashboard

	dash.OrgID = dto.OrgID
	dash.Title = strings.TrimSpace(dash.Title)
	dash.Data.Set("title", dash.Title)
	dash.SetUID(strings.TrimSpace(dash.UID))

	if dash.Title == "" {
		return nil, dashboards.ErrDashboardTitleEmpty
	}

	if dash.IsFolder && dash.FolderID > 0 {
		return nil, dashboards.ErrDashboardFolderCannotHaveParent
	}

	if dash.IsFolder && strings.EqualFold(dash.Title, dashboards.RootFolderName) {
		return nil, dashboards.ErrDashboardFolderNameExists
	}

	if !util.IsValidShortUID(dash.UID) {
		return nil, dashboards.ErrDashboardInvalidUid
	} else if util.IsShortUIDTooLong(dash.UID) {
		return nil, dashboards.ErrDashboardUidTooLong
	}

	_, err := s.dashboardStore.ValidateDashboardBeforeSave(ctx, dash, dto.Overwrite)
	if err != nil {
		return nil, err
	}

	guard, err := getGuardianForSavePermissionCheck(ctx, dash, dto.User)
	if err != nil {
		return nil, err
	}

	if dash.ID == 0 {
		if canCreate, err := guard.CanCreate(dash.FolderID, dash.IsFolder); err != nil || !canCreate {
			if err != nil {
				return nil, err
			}
			return nil, dashboards.ErrDashboardUpdateAccessDenied
		}
	} else {
		if canSave, err := guard.CanSave(); err != nil || !canSave {
			if err != nil {
				return nil, err
			}
			return nil, dashboards.ErrDashboardUpdateAccessDenied
		}
	}

	userID := int64(0)
	namespaceID, userIDstr := dto.User.GetNamespacedID()
	if namespaceID != identity.NamespaceUser && namespaceID != identity.NamespaceServiceAccount {
		s.log.Warn("User does not belong to a user or service account namespace, using 0 as user ID", "namespaceID", namespaceID, "userID", userIDstr)
	} else {
		userID, err = identity.IntIdentifier(namespaceID, userIDstr)
		if err != nil {
			s.log.Warn("failed to parse user ID", "namespaceID", namespaceID, "userID", userIDstr, "error", err)
		}
	}

	cmd := &dashboards.SaveDashboardCommand{
		Dashboard: dash.Data,
		Message:   dto.Message,
		OrgID:     dto.OrgID,
		Overwrite: dto.Overwrite,
		UserID:    userID,
		FolderID:  dash.FolderID,
		FolderUID: dash.FolderUID,
		IsFolder:  dash.IsFolder,
		PluginID:  dash.PluginID,
	}

	if !dto.UpdatedAt.IsZero() {
		cmd.UpdatedAt = dto.UpdatedAt
	}

	return cmd, nil
}

// getGuardianForSavePermissionCheck returns the guardian to be used for checking permission of dashboard
// It replaces deleted Dashboard.GetDashboardIdForSavePermissionCheck()
func getGuardianForSavePermissionCheck(ctx context.Context, d *dashboards.Dashboard, user identity.Requester) (guardian.DashboardGuardian, error) {
	newDashboard := d.ID == 0

	if newDashboard {
		// if it's a new dashboard/folder check the parent folder permissions
		guard, err := guardian.New(ctx, d.FolderID, d.OrgID, user)
		if err != nil {
			return nil, err
		}
		return guard, nil
	}
	guard, err := guardian.NewByDashboard(ctx, d, d.OrgID, user)
	if err != nil {
		return nil, err
	}
	return guard, nil
}

func (s *Service) nestedFolderCreate(ctx context.Context, cmd *folder.CreateFolderCommand) (*folder.Folder, error) {
	if cmd.ParentUID != "" {
		if err := s.validateParent(ctx, cmd.OrgID, cmd.ParentUID, cmd.UID); err != nil {
			return nil, err
		}
	}
	return s.store.Create(ctx, *cmd)
}

func (s *Service) validateParent(ctx context.Context, orgID int64, parentUID string, UID string) error {
	ancestors, err := s.store.GetParents(ctx, folder.GetParentsQuery{UID: parentUID, OrgID: orgID})
	if err != nil {
		return fmt.Errorf("failed to get parents: %w", err)
	}

	if len(ancestors) == folder.MaxNestedFolderDepth {
		return folder.ErrMaximumDepthReached.Errorf("failed to validate parent folder")
	}

	// Create folder under itself is not allowed
	if parentUID == UID {
		return folder.ErrCircularReference
	}

	// check there is no circular reference
	for _, ancestor := range ancestors {
		if ancestor.UID == UID {
			return folder.ErrCircularReference
		}
	}

	return nil
}

func toFolderError(err error) error {
	if errors.Is(err, dashboards.ErrDashboardTitleEmpty) {
		return dashboards.ErrFolderTitleEmpty
	}

	if errors.Is(err, dashboards.ErrDashboardUpdateAccessDenied) {
		return dashboards.ErrFolderAccessDenied
	}

	if errors.Is(err, dashboards.ErrDashboardWithSameNameInFolderExists) {
		return dashboards.ErrFolderSameNameExists
	}

	if errors.Is(err, dashboards.ErrDashboardWithSameUIDExists) {
		return dashboards.ErrFolderWithSameUIDExists
	}

	if errors.Is(err, dashboards.ErrDashboardVersionMismatch) {
		return dashboards.ErrFolderVersionMismatch
	}

	if errors.Is(err, dashboards.ErrDashboardNotFound) {
		return dashboards.ErrFolderNotFound
	}

	return err
}

func (s *Service) RegisterService(r folder.RegistryService) error {
	s.mutex.Lock()
	defer s.mutex.Unlock()

	s.registry[r.Kind()] = r

	return nil
}
