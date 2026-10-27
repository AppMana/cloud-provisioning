package upstream

import _ "embed"

//go:generate ./vpc-resource-controller/render.sh

// VPCResourceController is the Deployment of AWS's VPC resource controller at
// ca7792b, rendered from the vendored config/default (vpc-resource-controller/).
// The rest of that render is the product chart's templates/aws-windows-ipam.yaml.
//
//go:embed vpc-resource-controller/deployment.yaml
var VPCResourceController []byte
