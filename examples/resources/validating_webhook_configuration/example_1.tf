# Copyright (c) HashiCorp, Inc.
# SPDX-License-Identifier: MPL-2.0

resource "kubernetes_validating_webhook_configuration" "example" {
  metadata {
    name = "test.terraform.io"
  }

  webhook {
    name = "test.terraform.io"

    admission_review_versions = ["v1", "v1beta1"]

    client_config {
      service {
        namespace = "example-namespace"
        name      = "example-service"
      }
    }

    rule {
      api_groups   = ["apps"]
      api_versions = ["v1"]
      operations   = ["CREATE"]
      resources    = ["deployments"]
      scope        = "Namespaced"
    }

    side_effects = "None"
  }
}
