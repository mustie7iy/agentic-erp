# tflint for every module and root (scripts/deploy-checks.sh, ADR-0035):
# the bundled Terraform ruleset's recommended rules, and every variable and
# output documented.
plugin "terraform" {
  enabled = true
  preset  = "recommended"
}

rule "terraform_documented_variables" {
  enabled = true
}

rule "terraform_documented_outputs" {
  enabled = true
}

rule "terraform_naming_convention" {
  enabled = true
}
