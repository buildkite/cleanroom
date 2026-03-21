# Intentionally left empty.
#
# Use an explicit region-matched var file with the matching Terraform workspace:
#   terraform workspace select -or-create ap-southeast-2
#   terraform plan -var-file=prod.ap-southeast-2.tfvars
#
#   terraform workspace select -or-create us-west-2
#   terraform plan -var-file=prod.us-west-2.tfvars
