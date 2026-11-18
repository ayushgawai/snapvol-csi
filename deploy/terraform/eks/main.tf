# Example EKS cluster for snapvol-csi. NOT applied by this repo's tooling:
#   cd deploy/terraform/eks && terraform init && terraform apply
# Requires AWS credentials. Creates a VPC, an EKS cluster, and one node group.
terraform {
  required_version = ">= 1.5"
  required_providers { aws = { source = "hashicorp/aws", version = "~> 5.0" } }
}
variable "region" { default = "us-east-1" }
variable "cluster_name" { default = "snapvol-eks" }

provider "aws" { region = var.region }
data "aws_availability_zones" "up" { state = "available" }

resource "aws_vpc" "this" {
  cidr_block           = "10.0.0.0/16"
  enable_dns_hostnames = true
}
resource "aws_subnet" "this" {
  count                   = 2
  vpc_id                  = aws_vpc.this.id
  cidr_block              = "10.0.${count.index}.0/24"
  availability_zone       = data.aws_availability_zones.up.names[count.index]
  map_public_ip_on_launch = true
}
resource "aws_internet_gateway" "this" { vpc_id = aws_vpc.this.id }
resource "aws_route_table" "this" {
  vpc_id = aws_vpc.this.id
  route { cidr_block = "0.0.0.0/0", gateway_id = aws_internet_gateway.this.id }
}
resource "aws_route_table_association" "this" {
  count          = 2
  subnet_id      = aws_subnet.this[count.index].id
  route_table_id = aws_route_table.this.id
}

data "aws_iam_policy_document" "eks_assume" {
  statement {
    actions = ["sts:AssumeRole"]
    principals { type = "Service", identifiers = ["eks.amazonaws.com", "ec2.amazonaws.com"] }
  }
}
resource "aws_iam_role" "cluster" {
  name               = "${var.cluster_name}-cluster"
  assume_role_policy = data.aws_iam_policy_document.eks_assume.json
}
resource "aws_iam_role_policy_attachment" "cluster" {
  role       = aws_iam_role.cluster.name
  policy_arn = "arn:aws:iam::aws:policy/AmazonEKSClusterPolicy"
}
resource "aws_iam_role" "node" {
  name               = "${var.cluster_name}-node"
  assume_role_policy = data.aws_iam_policy_document.eks_assume.json
}
resource "aws_iam_role_policy_attachment" "node" {
  for_each = toset(["AmazonEKSWorkerNodePolicy", "AmazonEKS_CNI_Policy", "AmazonEC2ContainerRegistryReadOnly"])
  role       = aws_iam_role.node.name
  policy_arn = "arn:aws:iam::aws:policy/${each.value}"
}

resource "aws_eks_cluster" "this" {
  name     = var.cluster_name
  role_arn = aws_iam_role.cluster.arn
  vpc_config { subnet_ids = aws_subnet.this[*].id }
}
resource "aws_eks_node_group" "this" {
  cluster_name    = aws_eks_cluster.this.name
  node_group_name = "default"
  node_role_arn   = aws_iam_role.node.arn
  subnet_ids      = aws_subnet.this[*].id
  instance_types  = ["t3.large"]
  scaling_config { desired_size = 2, max_size = 3, min_size = 1 }
}
output "kubeconfig" {
  value = "aws eks update-kubeconfig --region ${var.region} --name ${aws_eks_cluster.this.name}"
}
output "next" {
  value = "helm install snapvol ../../../deploy/helm -n kube-system --set backend=fsx"
}
