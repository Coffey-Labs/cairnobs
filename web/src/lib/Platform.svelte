<script lang="ts">
	// What a cluster, array or VM runs on (#80): a small label with a dot
	// whose color tells the families apart at a glance: Kubernetes and
	// OpenShift, VMware, Nutanix, Proxmox, Hyper-V, KVM, Pure, IBM.
	let { p = '' }: { p?: string } = $props();
	const family = (p: string) =>
		p === 'OpenShift' || p === 'OKD' ? 'openshift'
		: p === 'Kubernetes' ? 'k8s'
		: p.startsWith('VMware') ? 'vmware'
		: p === 'Nutanix' ? 'nutanix'
		: p.startsWith('Proxmox') ? 'proxmox'
		: p === 'Hyper-V' ? 'hyperv'
		: p === 'KVM' ? 'kvm'
		: p.startsWith('Pure') ? 'pure'
		: p.startsWith('IBM') ? 'ibm'
		: 'other';
</script>

{#if p}<span class="plat {family(p)}" title="Runs on {p}">{p}</span>{/if}

<style>
	.plat { display: inline-flex; align-items: center; gap: 5px; padding: 1px 7px 1px 6px; border: 1px solid var(--border); border-radius: 999px;
		font-size: 11px; font-weight: 600; line-height: 16px; color: var(--foreground); white-space: nowrap; vertical-align: middle; background: var(--card); }
	.plat::before { content: ''; width: 7px; height: 7px; border-radius: 50%; background: var(--dot, var(--muted-fg)); flex: none; }
	.k8s { --dot: #4f7fe0; }
	.openshift { --dot: #e0453a; }
	.vmware { --dot: #3fa45b; }
	.nutanix { --dot: #8a5cd6; }
	.proxmox { --dot: #e2861b; }
	.hyperv { --dot: #1f9bd6; }
	.kvm { --dot: #c9a227; }
	.pure { --dot: #f26b2a; }
	.ibm { --dot: #3b6fd1; }
</style>
