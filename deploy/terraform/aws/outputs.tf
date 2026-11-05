output "ipv4_address" {
  description = "Point the domain, its wildcard, and the dashboard host at this address."
  value       = aws_eip.wherry.public_ip
}

output "ssh_command" {
  description = "Admin login. On AWS it is ubuntu with sudo, not root."
  value       = "ssh ubuntu@${aws_eip.wherry.public_ip}"
}
