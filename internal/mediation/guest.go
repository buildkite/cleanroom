package mediation

// GuestHostname is the guest-visible hostname of the lineage gateway's bound
// service endpoint. SporeVM derives it from the bound-service name as
// NAME.spore.internal; the CLI declaration syntax offers no override.
const GuestHostname = BoundServiceName + ".spore.internal"

// GuestPort is the guest-visible port of the lineage gateway.
const GuestPort uint16 = 8170

// BoundServiceName is the stable bound-service name recorded in provenance
// and used in spore --bind-service declarations and bindings.
const BoundServiceName = "cleanroom-gateway"
