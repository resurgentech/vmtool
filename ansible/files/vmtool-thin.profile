# LVM profile for vmtool's thin pool (installed as
# /etc/lvm/profile/vmtool-thin.profile by ansible/playbooks/tasks/nvme_stripe.yml).
# lvm2-monitor (dmeventd) grows the pool by 10 % once it is 80 % full.
activation {
	thin_pool_autoextend_threshold = 80
	thin_pool_autoextend_percent = 10
}
