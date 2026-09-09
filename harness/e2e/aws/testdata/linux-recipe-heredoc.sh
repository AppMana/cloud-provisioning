# Minimal replay of the native recipe's corrupted opening delimiter.
python3 - <<'CLDT_8b5d985f803df27acb44f900b2574a5b48e600bd2f87a3335d2a853b888e9298RED_PREPARE'
print('preparation receipt')
CLDT_SHARED_PREPARE
touch /var/lib/cloud-provisioning-image-ready
