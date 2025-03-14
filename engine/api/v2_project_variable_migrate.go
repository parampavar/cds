package api

import (
	"context"
	"net/http"

	"github.com/gorilla/mux"

	"github.com/ovh/cds/engine/api/application"
	"github.com/ovh/cds/engine/api/environment"
	"github.com/ovh/cds/engine/api/project"
	"github.com/ovh/cds/engine/service"
	"github.com/ovh/cds/sdk"
	"github.com/ovh/cds/sdk/slug"
)

func (api *API) postMigrateAsCodeVariableToVariableSetItemHandler() ([]service.RbacChecker, service.Handler) {
	return service.RBAC(api.projectManage),
		func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
			vars := mux.Vars(req)
			pKey := vars["projectKey"]

			var copyRequest sdk.CopyAsCodeSecretToVariableSet
			if err := service.UnmarshalBody(req, &copyRequest); err != nil {
				return err
			}

			proj, err := project.Load(ctx, api.mustDB(), pKey)
			if err != nil {
				return err
			}

			secretValue, err := project.DecryptWithBuiltinKey(ctx, api.mustDB(), proj.ID, copyRequest.AsCodeIdentifier)
			if err != nil {
				return err
			}

			tx, err := api.mustDB().Begin()
			if err != nil {
				return sdk.WithStack(err)
			}
			defer tx.Rollback() //nolint

			vs, err := project.LoadVariableSetByName(ctx, tx, pKey, copyRequest.VariableSetName)
			if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
				return err
			}

			if sdk.ErrorIs(err, sdk.ErrNotFound) {
				vs = &sdk.ProjectVariableSet{
					Name:       copyRequest.VariableSetName,
					ProjectKey: pKey,
				}
				if err := project.InsertVariableSet(ctx, tx, vs); err != nil {
					return err
				}
			}

			it := &sdk.ProjectVariableSetItem{
				ProjectVariableSetID: vs.ID,
				Name:                 slug.Convert(copyRequest.VariableSetItemName),
				Type:                 sdk.ProjectVariableTypeSecret,
				Value:                secretValue,
			}
			if err := project.InsertVariableSetItemSecret(ctx, tx, it); err != nil {
				return err
			}

			if err := tx.Commit(); err != nil {
				return sdk.WithStack(err)
			}
			return service.WriteJSON(w, nil, http.StatusOK)
		}
}

func (api *API) postMigrateEnvironmentVariableToVariableSetHandler() ([]service.RbacChecker, service.Handler) {
	return service.RBAC(api.projectManage),
		func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
			vars := mux.Vars(req)
			pKey := vars["projectKey"]

			var copyRequest sdk.CopyEnvironmentVariableToVariableSet
			if err := service.UnmarshalBody(req, &copyRequest); err != nil {
				return err
			}

			env, err := environment.LoadEnvironmentByName(api.mustDB(), pKey, copyRequest.EnvironmentName)
			if err != nil {
				return err
			}
			envVars, err := environment.LoadAllVariablesWithDecrytion(api.mustDB(), env.ID)
			if err != nil {
				return err
			}

			tx, err := api.mustDB().Begin()
			if err != nil {
				return sdk.WithStack(err)
			}
			defer tx.Rollback() //nolint

			vs, err := project.LoadVariableSetByName(ctx, tx, pKey, copyRequest.VariableSetName)
			if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
				return err
			}
			if sdk.ErrorIs(err, sdk.ErrNotFound) {
				vs = &sdk.ProjectVariableSet{
					Name:       copyRequest.VariableSetName,
					ProjectKey: pKey,
				}
				if err := project.InsertVariableSet(ctx, tx, vs); err != nil {
					return err
				}
			}

			for _, v := range envVars {
				itemType := sdk.ProjectVariableTypeString
				if v.Type == sdk.SecretVariable {
					itemType = sdk.ProjectVariableTypeSecret
				}
				it := &sdk.ProjectVariableSetItem{
					ProjectVariableSetID: vs.ID,
					Name:                 slug.Convert(v.Name),
					Type:                 itemType,
					Value:                v.Value,
				}
				switch v.Type {
				case sdk.SecretVariable:
					if err := project.InsertVariableSetItemSecret(ctx, tx, it); err != nil {
						return err
					}
				default:
					if err := project.InsertVariableSetItemText(ctx, tx, it); err != nil {
						return err
					}
				}
			}
			if err := tx.Commit(); err != nil {
				return sdk.WithStack(err)
			}
			return service.WriteJSON(w, nil, http.StatusOK)
		}
}

func (api *API) postMigrateApplicationIntegrationVariableToVariableSetHandler() ([]service.RbacChecker, service.Handler) {
	return service.RBAC(api.projectManage),
		func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
			vars := mux.Vars(req)
			pKey := vars["projectKey"]

			var copyRequest sdk.CopyApplicationIntegrationVariableToVariableSet
			if err := service.UnmarshalBody(req, &copyRequest); err != nil {
				return err
			}

			app, err := application.LoadByName(ctx, api.mustDB(), pKey, copyRequest.ApplicationName, application.LoadOptions.WithClearDeploymentStrategies)
			if err != nil {
				return err
			}

			var value, itemType string

			for k, integ := range app.DeploymentStrategies {
				if k != copyRequest.IntegrationName {
					continue
				}
				for varName, config := range integ {
					if varName != copyRequest.VariableName {
						continue
					}
					value = config.Value
					if config.Type == sdk.IntegrationConfigTypePassword {
						itemType = sdk.ProjectVariableTypeSecret
					} else {
						itemType = sdk.ProjectVariableTypeString
					}
				}
			}
			if value == "" {
				return sdk.NewErrorFrom(sdk.ErrInvalidData, "variable %s not found", copyRequest.VariableName)
			}

			tx, err := api.mustDB().Begin()
			if err != nil {
				return sdk.WithStack(err)
			}
			defer tx.Rollback() //nolint

			vs, err := project.LoadVariableSetByName(ctx, api.mustDB(), pKey, copyRequest.VariableSetName)
			if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
				return err
			}
			if sdk.ErrorIs(err, sdk.ErrNotFound) {
				vs = &sdk.ProjectVariableSet{
					Name:       copyRequest.VariableSetName,
					ProjectKey: pKey,
				}
				if err := project.InsertVariableSet(ctx, tx, vs); err != nil {
					return err
				}
			}

			it := &sdk.ProjectVariableSetItem{
				ProjectVariableSetID: vs.ID,
				Name:                 slug.Convert(copyRequest.VariableSetItemName),
				Type:                 itemType,
				Value:                value,
			}
			switch it.Type {
			case sdk.ProjectVariableTypeSecret:
				if err := project.InsertVariableSetItemSecret(ctx, tx, it); err != nil {
					return err
				}
			default:
				if err := project.InsertVariableSetItemText(ctx, tx, it); err != nil {
					return err
				}
			}

			if err := tx.Commit(); err != nil {
				return sdk.WithStack(err)
			}
			return service.WriteJSON(w, nil, http.StatusOK)

		}
}
func (api *API) postMigrateApplicationVariableToVariableSetHandler() ([]service.RbacChecker, service.Handler) {
	return service.RBAC(api.projectManage),
		func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
			vars := mux.Vars(req)
			pKey := vars["projectKey"]

			var copyRequest sdk.CopyApplicationVariableToVariableSet
			if err := service.UnmarshalBody(req, &copyRequest); err != nil {
				return err
			}

			app, err := application.LoadByName(ctx, api.mustDB(), pKey, copyRequest.ApplicationName, application.LoadOptions.WithVariablesWithClearPassword)
			if err != nil {
				return err
			}

			tx, err := api.mustDB().Begin()
			if err != nil {
				return sdk.WithStack(err)
			}
			defer tx.Rollback() //nolint

			vs, err := project.LoadVariableSetByName(ctx, api.mustDB(), pKey, copyRequest.VariableSetName)
			if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
				return err
			}
			if sdk.ErrorIs(err, sdk.ErrNotFound) {
				vs = &sdk.ProjectVariableSet{
					Name:       copyRequest.VariableSetName,
					ProjectKey: pKey,
				}
				if err := project.InsertVariableSet(ctx, tx, vs); err != nil {
					return err
				}
			}

			for _, v := range app.Variables {
				itemType := sdk.ProjectVariableTypeString
				if v.Type == sdk.SecretVariable {
					itemType = sdk.ProjectVariableTypeSecret
				}
				it := &sdk.ProjectVariableSetItem{
					ProjectVariableSetID: vs.ID,
					Name:                 slug.Convert(v.Name),
					Type:                 itemType,
					Value:                v.Value,
				}
				switch v.Type {
				case sdk.SecretVariable:
					if err := project.InsertVariableSetItemSecret(ctx, tx, it); err != nil {
						return err
					}
				default:
					if err := project.InsertVariableSetItemText(ctx, tx, it); err != nil {
						return err
					}
				}
			}
			if err := tx.Commit(); err != nil {
				return sdk.WithStack(err)
			}
			return service.WriteJSON(w, nil, http.StatusOK)
		}
}

func (api *API) postMigrateProjectVariableHandler() ([]service.RbacChecker, service.Handler) {
	return service.RBAC(api.projectManage),
		func(ctx context.Context, w http.ResponseWriter, req *http.Request) error {
			vars := mux.Vars(req)
			pKey := vars["projectKey"]
			force := service.FormBool(req, "force")

			var copyRequest sdk.CopyProjectVariableToVariableSet
			if err := service.UnmarshalBody(req, &copyRequest); err != nil {
				return err
			}
			if copyRequest.NewName == "" {
				copyRequest.NewName = copyRequest.VariableName
			}

			proj, err := project.Load(ctx, api.mustDB(), pKey, project.LoadOptions.WithVariablesWithClearPassword)
			if err != nil {
				return err
			}

			tx, err := api.mustDB().Begin()
			if err != nil {
				return sdk.WithStack(err)
			}
			defer tx.Rollback() //nolint

			vs, err := project.LoadVariableSetByName(ctx, api.mustDB(), pKey, copyRequest.VariableSetName)
			if err != nil && !sdk.ErrorIs(err, sdk.ErrNotFound) {
				return err
			}
			if sdk.ErrorIs(err, sdk.ErrNotFound) {
				if force {
					vs = &sdk.ProjectVariableSet{
						Name:       copyRequest.VariableSetName,
						ProjectKey: pKey,
					}
					if err := project.InsertVariableSet(ctx, tx, vs); err != nil {
						return err
					}
				} else {
					return sdk.NewErrorFrom(sdk.ErrNotFound, "Variable set %s doesn't exist", copyRequest.VariableSetName)
				}
			}

			for _, v := range proj.Variables {
				if v.Name == copyRequest.VariableName {
					itemType := sdk.ProjectVariableTypeString
					if v.Type == sdk.SecretVariable {
						itemType = sdk.ProjectVariableTypeSecret
					}
					it := &sdk.ProjectVariableSetItem{
						ProjectVariableSetID: vs.ID,
						Name:                 copyRequest.NewName,
						Type:                 itemType,
						Value:                v.Value,
					}
					switch v.Type {
					case sdk.SecretVariable:
						if err := project.InsertVariableSetItemSecret(ctx, tx, it); err != nil {
							return err
						}
					default:
						if err := project.InsertVariableSetItemText(ctx, tx, it); err != nil {
							return err
						}
					}
					break
				}
			}

			if err := tx.Commit(); err != nil {
				return sdk.WithStack(err)
			}
			return service.WriteJSON(w, nil, http.StatusOK)
		}
}
