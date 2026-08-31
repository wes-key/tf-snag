# tf-snag:ignore-drift  reason: managed by PIM outside Terraform
resource "azurerm_role_assignment" "admin" {
  scope = "/subscriptions/x"
}

resource "azurerm_signalr_service" "legacy" {
  # tf-snag:ignore-deprecation reason: 4.0 upgrade in backlog
  live_trace_enabled = true
}
