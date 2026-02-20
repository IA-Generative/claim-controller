package controller

const (
	ManagedByLabelKey             = "claim-controller.io/managed-by"
	ManagedByLabelValue           = "claim-controller"
	ClaimLabelKey                 = "claim-controller.io/claim"
	ClaimLabelKeyId               = "claim-controller.io/claim.id"
	ExpiresAtAnnotationKey        = "claim-controller.io/expires-at"
	ClaimedAtAnnotationKey        = "claim-controller.io/claimed-at"
	CreatedByAnnotationKey        = "claim-controller.io/created-by"
	CreatedByAnnotationValue      = "claim-controller"
	PreProvisionedAnnotationKey   = "claim-controller.io/pre-provisioned"
	LazyProvisioningAnnotationKey = "claim.controller/lazy-provisionning"
	RenderedResourcesDataKey      = "renderedResources"
	ReturnValuesDataKey           = "returnValues"
	ClaimStatusDataKey            = "claimStatus"
	ClaimStatusMessageDataKey     = "claimStatusMessage"
	ClaimResourcesStatusDataKey   = "claimResourcesStatus"
)
