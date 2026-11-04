output "ipv4_address" {
  description = "Point the domain and its wildcard at this address."
  value       = hcloud_server.wherry.ipv4_address
}
