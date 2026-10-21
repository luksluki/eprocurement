package server

import (
	"context"
	"errors"
	"log/slog"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	procurementv1 "github.com/luksluki/eprocurement/gen/go/procurement/v1"
	"github.com/luksluki/eprocurement/internal/platform/rbac"
	"github.com/luksluki/eprocurement/services/procurement/vendor"
)

// api adapts the vendor domain to the procurement.v1 gRPC contract. Every
// method reads the caller identity the gateway propagated (tenant scoping,
// audit ids); the RBAC interceptor has already re-checked the permission.
type api struct {
	procurementv1.UnimplementedProcurementServer
	store *vendor.Store
	log   *slog.Logger
}

// authTenant returns the caller's auth context, rejecting calls that reached
// this far without one.
func authTenant(ctx context.Context) (*rbac.AuthContext, error) {
	auth := rbac.FromMetadata(ctx)
	if auth == nil || auth.TenantID == "" {
		return nil, status.Error(codes.Unauthenticated, "auth context missing: no caller tenant")
	}
	return auth, nil
}

func (a *api) CreateVendor(ctx context.Context, req *procurementv1.CreateVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := vendor.ValidateCreate(vendor.Normalize(req.GetCode()), req.GetName(), req.GetContactEmail()); err != nil {
		return nil, mapVendorError(err)
	}
	v, err := a.store.Create(ctx, auth.TenantID, vendor.CreateInput{
		Code: req.GetCode(), Name: req.GetName(),
		ContactEmail: req.GetContactEmail(), TaxID: req.GetTaxId(),
		UserID: auth.UserID,
	})
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func (a *api) GetVendor(ctx context.Context, req *procurementv1.GetVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	v, err := a.store.Get(ctx, auth.TenantID, req.GetId())
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func (a *api) UpdateVendor(ctx context.Context, req *procurementv1.UpdateVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	if err := vendor.ValidateUpdate(req.GetName(), req.GetContactEmail()); err != nil {
		return nil, mapVendorError(err)
	}
	v, err := a.store.Update(ctx, auth.TenantID, vendor.UpdateInput{
		ID: req.GetId(), Name: req.GetName(),
		ContactEmail: req.GetContactEmail(), TaxID: req.GetTaxId(),
		UserID: auth.UserID,
	})
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func (a *api) DeactivateVendor(ctx context.Context, req *procurementv1.DeactivateVendorRequest) (*procurementv1.Vendor, error) {
	auth, err := authTenant(ctx)
	if err != nil {
		return nil, err
	}
	v, err := a.store.Deactivate(ctx, auth.TenantID, req.GetId(), auth.UserID)
	if err != nil {
		return nil, mapVendorError(err)
	}
	return vendorPB(v), nil
}

func mapVendorError(err error) error {
	switch {
	case errors.Is(err, vendor.ErrCodeInvalid),
		errors.Is(err, vendor.ErrNameInvalid),
		errors.Is(err, vendor.ErrEmailInvalid),
		errors.Is(err, vendor.ErrIDInvalid):
		return status.Error(codes.InvalidArgument, err.Error())
	case errors.Is(err, vendor.ErrDuplicateCode):
		return status.Error(codes.AlreadyExists, err.Error())
	case errors.Is(err, vendor.ErrNotFound):
		return status.Error(codes.NotFound, err.Error())
	case errors.Is(err, vendor.ErrDeactivated):
		return status.Error(codes.FailedPrecondition, err.Error())
	default:
		return status.Error(codes.Internal, "internal error")
	}
}

func vendorPB(v vendor.Vendor) *procurementv1.Vendor {
	return &procurementv1.Vendor{
		Id: v.ID, TenantId: v.TenantID, Code: v.Code, Name: v.Name,
		ContactEmail: v.ContactEmail, TaxId: v.TaxID, Status: v.Status,
		CreatedAt: vendor.RFC3339(v.CreatedAt), CreatedBy: v.CreatedBy,
		UpdatedAt: vendor.RFC3339(v.UpdatedAt), UpdatedBy: v.UpdatedBy,
	}
}
