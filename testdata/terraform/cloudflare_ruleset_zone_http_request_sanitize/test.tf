resource "cloudflare_ruleset" "terraform_managed_resource" {
  kind    = "zone"
  name    = "Zone sanitize ruleset"
  phase   = "http_request_sanitize"
  zone_id = "0da42c8d2132a9ddaf714f9e7c920711"
  rules {
    action     = "execute"
    enabled    = true
    expression = "true"
    action_parameters {
      id      = "70339d97bdb34195bbf054b1ebe81f76"
      version = "latest"
      overrides {
        rules {
          id     = "78723a9e0c7c4c6dbec5684cb766231d"
          status = "enabled"
        }
      }
    }
  }
}
