output "instance_id" {
  description = "EC2 instance ID, usable with AWS Systems Manager Session Manager."
  value       = aws_instance.app.id
}

output "public_ip" {
  description = "Stable Elastic IP address."
  value       = aws_eip.app.public_ip
}

output "application_url" {
  description = "Expected application URL. HTTPS becomes available after the application and Caddy are started."
  value       = var.domain_name == null ? "http://${aws_eip.app.public_ip}" : "https://${var.domain_name}"
}

output "session_manager_command" {
  description = "Connect without opening SSH. Requires the Session Manager plugin locally."
  value       = "aws ssm start-session --target ${aws_instance.app.id} --region ${var.region}"
}

