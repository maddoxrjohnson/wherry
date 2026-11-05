output "ipv4_address" {
  description = "Point the domain, its wildcard, and the dashboard host at this address."
  value       = hcloud_server.wherry.ipv4_address
}
