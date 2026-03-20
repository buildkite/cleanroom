# Intentionally left empty.
#
# Use an explicit region-matched var file with the matching Terraform workspace:
#   terraform workspace select ap-southeast-2
#   terraform plan -var-file=prod.ap-southeast-2.tfvars
#
#   terraform workspace select us-west-2
#   terraform plan -var-file=prod.us-west-2.tfvars
