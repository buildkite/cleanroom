data "aws_caller_identity" "current" {}

data "aws_partition" "current" {}

locals {
  common_tags = merge(var.tags, {
    Name = var.name_prefix
  })

  parameter_names = compact([
    var.buildkite_token_parameter_name,
    var.tailscale_auth_key_parameter_name,
    var.git_deploy_key_parameter_name,
  ])

  parameter_arns = [
    for name in local.parameter_names :
    "arn:${data.aws_partition.current.partition}:ssm:${var.aws_region}:${data.aws_caller_identity.current.account_id}:parameter/${trimprefix(name, "/")}"
  ]
}

resource "aws_iam_role" "host" {
  name = "${var.name_prefix}-host-role"

  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Principal = {
        Service = "ec2.amazonaws.com"
      }
      Action = "sts:AssumeRole"
    }]
  })

  tags = local.common_tags
}

resource "aws_iam_role_policy_attachment" "ssm_core" {
  role       = aws_iam_role.host.name
  policy_arn = "arn:${data.aws_partition.current.partition}:iam::aws:policy/AmazonSSMManagedInstanceCore"
}

resource "aws_iam_role_policy" "parameter_read" {
  name = "${var.name_prefix}-parameter-read"
  role = aws_iam_role.host.id

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [
      {
        Effect = "Allow"
        Action = [
          "ssm:GetParameter",
          "ssm:GetParameters",
        ]
        Resource = local.parameter_arns
      },
      {
        Effect = "Allow"
        Action = [
          "kms:Decrypt",
        ]
        Resource = "*"
        Condition = {
          StringEquals = {
            "kms:ViaService" = "ssm.${var.aws_region}.amazonaws.com"
          }
          "ForAnyValue:StringLike" = {
            "kms:EncryptionContext:PARAMETER_ARN" = local.parameter_arns
          }
        }
      },
    ]
  })
}

resource "aws_iam_instance_profile" "host" {
  name = "${var.name_prefix}-host-profile"
  role = aws_iam_role.host.name

  tags = local.common_tags
}

resource "aws_security_group" "host" {
  name        = "${var.name_prefix}-host"
  description = "No inbound access; outbound only for SSM/Tailscale/bootstrap"
  vpc_id      = var.vpc_id

  egress {
    from_port        = 0
    to_port          = 0
    protocol         = "-1"
    cidr_blocks      = ["0.0.0.0/0"]
    ipv6_cidr_blocks = ["::/0"]
  }

  tags = local.common_tags
}

resource "aws_instance" "host" {
  ami                    = var.ami_id
  instance_type          = var.instance_type
  subnet_id              = var.subnet_id
  vpc_security_group_ids = [aws_security_group.host.id]

  cpu_options {
    nested_virtualization = "enabled"
  }

  associate_public_ip_address = false
  iam_instance_profile        = aws_iam_instance_profile.host.name

  metadata_options {
    http_endpoint = "enabled"
    http_tokens   = "required"
  }

  root_block_device {
    volume_type = "gp3"
    volume_size = var.root_volume_size_gib
    encrypted   = true
  }

  user_data_replace_on_change = true
  user_data = templatefile("${path.module}/templates/user_data.sh.tftpl", {
    aws_region                        = var.aws_region
    name_prefix                       = var.name_prefix
    buildkite_token_parameter_name    = var.buildkite_token_parameter_name
    tailscale_auth_key_parameter_name = var.tailscale_auth_key_parameter_name
    git_deploy_key_parameter_name     = var.git_deploy_key_parameter_name
    repo_url                          = var.repo_url
    repo_ref                          = var.repo_ref
    setup_script_path                 = var.setup_script_path
    tailscale_version                 = var.tailscale_version
    tailscale_hostname_prefix         = var.tailscale_hostname_prefix
    tailscale_advertise_tags          = var.tailscale_advertise_tags
    tailscale_enable_ssh              = var.tailscale_enable_ssh
    tailscale_accept_routes           = var.tailscale_accept_routes
  })

  tags = local.common_tags
}
