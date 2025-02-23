// Copyright (c) 2016-2019 Cristian Măgherușan-Stanciu
// Licensed under the Open Software License version 3.0

package autospotting

import "log"

type target struct {
	asg              *autoScalingGroup
	totalInstances   int64
	onDemandInstance *instance
	spotInstance     *instance
}

type runer interface {
	run()
}

// No-op run
type skipRun struct {
	reason string
}

func (s skipRun) run() {}

// terminates a random spot instance after enabling the event-based logic
type terminateSpotInstance struct {
	target target
}

func (tsi terminateSpotInstance) run() {
	asg := tsi.target.asg
	asg.terminateRandomSpotInstanceIfHavingEnough(
		tsi.target.totalInstances, true)
}

// launches a spot instance replacement
type launchSpotReplacement struct {
	target target
}

func (lsr launchSpotReplacement) run() {
	spotInstanceID, err := lsr.target.onDemandInstance.launchSpotReplacement()
	if err != nil {
		log.Printf("Could not launch replacement spot instance: %s", err)
		return
	}
	log.Printf("Successfully launched spot instance %s, exiting...", *spotInstanceID)
}

type terminateUnneededSpotInstance struct {
	target target
}

func (tusi terminateUnneededSpotInstance) run() {
	asg := tusi.target.asg
	spotInstance := tusi.target.spotInstance
	spotInstanceID := *spotInstance.InstanceId

	log.Println("Spot instance", spotInstanceID, "is not need anymore by ASG",
		asg.name, "terminating the spot instance.")
	spotInstance.terminate()
}

type swapSpotInstance struct {
	target target
}

func (ssi swapSpotInstance) run() {
	asg := ssi.target.asg
	spotInstanceID := *ssi.target.spotInstance.InstanceId
	asg.replaceOnDemandInstanceWithSpot(spotInstanceID)
}

type sqsSendMessageOnInstanceLaunch struct {
	target target
}

func (ssmoil sqsSendMessageOnInstanceLaunch) run() {
	asg := ssmoil.target.asg
	onDemandInstanceID := ssmoil.target.onDemandInstance.InstanceId
	region := ssmoil.target.onDemandInstance.region
	state := ssmoil.target.onDemandInstance.State.Name
	region.sqsSendMessageOnInstanceLaunch(&asg.name, onDemandInstanceID, state, "cron-spot-instance-launch")
}
