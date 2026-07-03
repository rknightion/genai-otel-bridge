terraform {
  required_version = ">= 1.6.0"
  required_providers {
    aws = {
      # >= 6.34 required by terraform-aws-modules/ecs/aws ~> 7.0;
      # >= 5.98 required by terraform-aws-modules/dynamodb-table/aws ~> 5.0.
      # Both constraints are satisfied by AWS provider v6.x (current: 6.52+).
      source  = "hashicorp/aws"
      version = ">= 6.34.0"
    }
  }
}
